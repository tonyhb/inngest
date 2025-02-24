package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/go-multierror"
	"github.com/inngest/inngest/inngest"
	"github.com/inngest/inngest/pkg/config"
	"github.com/inngest/inngest/pkg/coredata"
	inmemorydatastore "github.com/inngest/inngest/pkg/coredata/inmemory"
	"github.com/inngest/inngest/pkg/event"
	"github.com/inngest/inngest/pkg/execution/queue"
	"github.com/inngest/inngest/pkg/execution/state"
	"github.com/inngest/inngest/pkg/expressions"
	"github.com/inngest/inngest/pkg/function"
	"github.com/inngest/inngest/pkg/logger"
	"github.com/inngest/inngest/pkg/pubsub"
	"github.com/inngest/inngest/pkg/service"
	"github.com/oklog/ulid/v2"
	"github.com/robfig/cron/v3"
)

type Opt func(s *svc)

func WithExecutionLoader(l coredata.ExecutionLoader) func(s *svc) {
	return func(s *svc) {
		s.data = l
	}
}

func NewService(c config.Config, opts ...Opt) service.Service {
	svc := &svc{config: c}
	for _, o := range opts {
		o(svc)
	}
	return svc
}

type svc struct {
	config config.Config
	// pubsub allows us to subscribe to new events, and re-publish events
	// if there are errors.
	pubsub pubsub.PublishSubscriber
	// data provides the required loading capabilities to trigger functions
	// from events.
	data coredata.ExecutionLoader
	// state allows the creation of new function runs.
	state state.Manager
	// queue allows the scheduling of new functions.
	queue queue.Queue
	// cronmanager allows the creation of new scheduled functions.
	cronmanager *cron.Cron
}

func (s svc) Name() string {
	return "runner"
}

func (s *svc) Pre(ctx context.Context) error {
	var err error

	if s.data == nil {
		s.data, err = inmemorydatastore.NewFSLoader(ctx, ".")
		if err != nil {
			return err
		}
	}

	logger.From(ctx).Info().Str("backend", s.config.Queue.Service.Backend).Msg("starting event stream")
	s.pubsub, err = pubsub.NewPublishSubscriber(ctx, s.config.EventStream.Service)
	if err != nil {
		return err
	}

	s.state, err = s.config.State.Service.Concrete.Manager(ctx)
	if err != nil {
		return err
	}

	logger.From(ctx).Info().Str("backend", s.config.Queue.Service.Backend).Msg("starting queue")
	s.queue, err = s.config.Queue.Service.Concrete.Queue()
	if err != nil {
		return err
	}

	// Each runner service is responsible for initializing cron-based executions.
	// As the runners are shared-nothing, there is contention when running multiple
	// services;  each individual service will attempt to create a new cron execution
	// simultaneously.  We currently rely on idempotency within the state store to
	// ensure that only one run can execute.
	//
	// In the future, we may want to add distributed locking and/or a limit on the
	// number of concurrent services that can schedule crons.  We don't really want
	// to rely on a single executor to 'claim' ownership:  we'd have to implement
	// more complex logic to check for the last heartbeat and valid cron scheduled,
	// then backtrack to re-execute in the case of node downtime.  This is simple.
	if err := s.initializeCrons(ctx); err != nil {
		return err
	}

	return nil
}

func (s *svc) Run(ctx context.Context) error {
	l := logger.From(ctx)
	l.Info().
		Str("topic", s.config.EventStream.Service.TopicName()).
		Msg("subscribing to events")
	err := s.pubsub.Subscribe(ctx, s.config.EventStream.Service.TopicName(), s.handleMessage)
	if err != nil {
		return err
	}
	return nil
}

func (s *svc) Stop(ctx context.Context) error {
	cronCtx := s.cronmanager.Stop()
	select {
	case <-cronCtx.Done():
	case <-ctx.Done():
		return fmt.Errorf("error waiting for scheduled executions to finish")
	}
	return nil
}

func (s *svc) initializeCrons(ctx context.Context) error {
	// If a previous cron manager exists, cancel it.
	if s.cronmanager != nil {
		s.cronmanager.Stop()
	}

	s.cronmanager = cron.New(
		cron.WithParser(
			cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
		),
	)

	// Set the functions within the engine, then iterate through each function's
	// triggers so that we can easily invoke them.  We also need to immediately
	// set up cron timers to invoke functions on a schedule.
	fns, err := s.data.FunctionsScheduled(ctx)
	if err != nil {
		return err
	}

	logger.From(ctx).
		Debug().
		Int("len", len(fns)).
		Msg("initializing scheduled messages")

	for _, f := range fns {
		fn := f
		// Set up a cron schedule for the current function.
		for _, t := range f.Triggers {
			if t.CronTrigger == nil {
				continue
			}
			_, err := s.cronmanager.AddFunc(t.Cron, func() {
				err := s.initialize(ctx, fn, event.Event{
					Name: "inngest/scheduled.timer",
				})
				if err != nil {
					logger.From(ctx).Error().Err(err).Msg("error initializing scheduled function")
				}
			})
			if err != nil {
				return err
			}
		}
	}

	s.cronmanager.Start()
	return nil
}

func (s *svc) handleMessage(ctx context.Context, m pubsub.Message) error {
	if m.Name != "event/event.received" {
		return fmt.Errorf("unknown event type: %s", m.Name)
	}

	evt := &event.Event{}
	if err := json.Unmarshal([]byte(m.Data), evt); err != nil {
		return fmt.Errorf("error unmarshalling event: %w", err)
	}

	l := logger.From(ctx).With().
		Str("event", evt.Name).
		Str("id", evt.ID).
		Logger()
	ctx = logger.With(ctx, l)

	l.Debug().Msg("received message")

	var errs error
	wg := &sync.WaitGroup{}

	// Trigger both new functions and pauses.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.functions(ctx, *evt); err != nil {
			l.Error().Err(err).Msg("error scheduling functions")
			errs = multierror.Append(errs, err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.pauses(ctx, *evt); err != nil {
			l.Error().Err(err).Msg("error consuming pauses")
			errs = multierror.Append(errs, err)
		}
	}()

	return errs
}

// functions triggers all functions from the given event.
func (s *svc) functions(ctx context.Context, evt event.Event) error {
	fns, err := s.data.FunctionsByTrigger(ctx, evt.Name)
	if err != nil {
		return fmt.Errorf("error loading functions by trigger: %w", err)
	}

	if len(fns) == 0 {
		return nil
	}

	logger.From(ctx).Debug().Int("len", len(fns)).Msg("scheduling functions")

	// Do this once instead of many times when evaluating expressions.
	evtMap := evt.Map()

	var errs error
	wg := &sync.WaitGroup{}
	for _, fn := range fns {
		// We want to initialize each function concurrently;  some of these
		// may have expressions that take ~tens of milliseconds to run, and
		// each function should have as little latency as possible.
		copied := fn
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, t := range copied.Triggers {
				if t.EventTrigger == nil || t.Event != evt.Name {
					// This isn't triggered by an event, so we skip this trigger entirely.
					continue
				}

				if t.Expression != nil {
					// Execute expressions here, ensuring that each function is only triggered
					// under the correct conditions.
					ok, _, evalerr := expressions.Evaluate(ctx, *t.Expression, map[string]interface{}{
						"event": evtMap,
					})
					if evalerr != nil {
						errs = multierror.Append(errs, evalerr)
						continue
					}
					if !ok {
						// Skip this trigger.
						continue
					}
				}

				// Initialize this function for this event only once;  we don't
				// want multiple matching triggers to run the function more than once.
				err := s.initialize(ctx, copied, evt)
				if err != nil {
					logger.From(ctx).Error().
						Err(err).
						Str("function", copied.ID).
						Msg("error initializing fn")
					errs = multierror.Append(errs, err)
				}
				return
			}
		}()
	}

	wg.Wait()
	return errs
}

// pauses searches for and triggers all pauses from this event.
func (s *svc) pauses(ctx context.Context, evt event.Event) error {
	logger.From(ctx).Trace().Msg("querying for pauses")
	iter, err := s.state.PausesByEvent(ctx, evt.Name)
	if err != nil {
		return fmt.Errorf("error finding event pauses: %w", err)
	}

	evtMap := evt.Map()
	for iter.Next(ctx) {
		pause := iter.Val(ctx)

		// NOTE: Some pauses may be nil or expired, as the iterator may take
		// time to process.  We handle that here and assume that the event
		// did not occur in time.
		if pause == nil || pause.Expires.Before(time.Now()) {
			continue
		}

		logger.From(ctx).Trace().
			Str("pause_id", pause.ID.String()).
			Msg("handling pause")

		if pause.Expression != nil {
			s, err := s.state.Load(ctx, pause.Identifier)
			if err != nil {
				return err
			}

			// Get expression data from the executor for the given run ID.
			data := state.EdgeExpressionData(ctx, s, pause.Outgoing)
			// Add the async event data to the expression
			data["async"] = evtMap
			// Compile and run the expression.
			ok, _, err := expressions.Evaluate(ctx, *pause.Expression, data)
			if err != nil {
				return err
			}
			if !ok {
				logger.From(ctx).Trace().
					Str("pause_id", pause.ID.String()).
					Str("expression", *pause.Expression).
					Msg("expression false")
				continue
			}
		}

		if pause.OnTimeout {
			// Delete this pause, as an event has occured which matches
			// the timeout.
			if err := s.state.ConsumePause(ctx, pause.ID); err != nil {
				return err
			}
		}

		logger.From(ctx).Debug().
			Str("pause_id", pause.ID.String()).
			Str("run_id", pause.Identifier.RunID.String()).
			Msg("leasing pause")

		// Lease this pause so that only this thread can schedule the execution.
		//
		// If we don't do this, there's a chance that two concurrent runners
		// attempt to enqueue the next step of the workflow.
		err := s.state.LeasePause(ctx, pause.ID)
		if err == state.ErrPauseLeased {
			// Ignore;  this is being handled by another runner.
			continue
		}
		if err != nil {
			return err
		}

		logger.From(ctx).Info().
			Str("pause_id", pause.ID.String()).
			Str("run_id", pause.Identifier.RunID.String()).
			Msg("resuming function")

		// Schedule an execution from the pause's entrypoint.
		if err := s.queue.Enqueue(
			ctx,
			queue.Item{
				Kind:       queue.KindEdge,
				Identifier: pause.Identifier,
				Payload: queue.PayloadEdge{
					Edge: inngest.Edge{
						Incoming: pause.Incoming,
					},
				},
			},
			time.Now(),
		); err != nil {
			return err
		}

		logger.From(ctx).Debug().
			Str("pause_id", pause.ID.String()).
			Str("run_id", pause.Identifier.RunID.String()).
			Msg("consuming pause")
		if err := s.state.ConsumePause(ctx, pause.ID); err != nil {
			return err
		}
	}

	return nil
}

func (s *svc) initialize(ctx context.Context, fn function.Function, evt event.Event) error {
	logger.From(ctx).Debug().Str("function", fn.ID).Msg("initializing fn")
	_, err := Initialize(ctx, fn, evt, s.state, s.queue)
	return err
}

// Initialize creates a new funciton run identifier for the given workflow and
// event, stores this in our state store, then enqueues a new function run
// within the given queue for execution.
//
// This is a separate, exported function so that it can be used from this service
// and also from eg. the run command.
func Initialize(ctx context.Context, fn function.Function, evt event.Event, s state.Manager, q queue.Producer) (*state.Identifier, error) {
	// XXX: This could/should be memoized.
	flow, err := fn.Workflow(ctx)
	if err != nil {
		return nil, err
	}

	zero := uuid.UUID{}
	if bytes.Equal(flow.UUID[:], zero[:]) {
		// Locally, we want to ensure that each function has its own deterministic
		// UUID for managing state.
		//
		// Using a remote API, this UUID may be a surrogate primary key.
		flow.UUID = function.DeterministicUUID(fn)
	}

	id := state.Identifier{
		WorkflowID: flow.UUID,
		RunID:      ulid.MustNew(ulid.Now(), rand.Reader),
		Key:        evt.ID,
	}

	if _, err := s.New(ctx, *flow, id, evt.Map()); err != nil {
		return nil, fmt.Errorf("error creating run state: %w", err)
	}

	// Enqueue running this from the source.
	err = q.Enqueue(ctx, queue.Item{
		Kind:       queue.KindEdge,
		Identifier: id,
		Payload:    queue.PayloadEdge{Edge: inngest.SourceEdge},
	}, time.Now())
	if err != nil {
		return &id, fmt.Errorf("error enqueuing function: %w", err)
	}

	return &id, nil
}
