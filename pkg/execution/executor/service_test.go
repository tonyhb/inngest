package executor

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/inngest/inngest/inngest"
	"github.com/inngest/inngest/pkg/config"
	_ "github.com/inngest/inngest/pkg/config/defaults"
	"github.com/inngest/inngest/pkg/coredata"
	inmemorydatastore "github.com/inngest/inngest/pkg/coredata/inmemory"
	"github.com/inngest/inngest/pkg/event"
	"github.com/inngest/inngest/pkg/execution/driver/mockdriver"
	"github.com/inngest/inngest/pkg/execution/queue"
	"github.com/inngest/inngest/pkg/execution/state"
	"github.com/inngest/inngest/pkg/function"
	"github.com/inngest/inngest/pkg/service"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"
)

const (
	timeout = 200 * time.Millisecond
	buffer  = 50 * time.Millisecond
)

type prepared struct {
	c  *config.Config
	q  queue.Queue
	sm state.Manager
	al coredata.ExecutionLoader
	f  function.Function
	w  inngest.Workflow
}

var (
	syncF = function.Function{
		ID:   "test",
		Name: "test",
		Triggers: []function.Trigger{
			{
				EventTrigger: &function.EventTrigger{
					Event: "test-evt",
				},
			},
		},
		Steps: map[string]function.Step{
			"1": {
				ID: "1",
				Runtime: inngest.RuntimeWrapper{
					Runtime: &mockdriver.Mock{},
				},
			},
			"2": {
				ID: "2",
				Runtime: inngest.RuntimeWrapper{
					Runtime: &mockdriver.Mock{},
				},
				After: []function.After{
					{Step: "1"},
				},
			},
			"3": {
				ID: "3",
				Runtime: inngest.RuntimeWrapper{
					Runtime: &mockdriver.Mock{},
				},
				After: []function.After{
					{
						Step: "2",
					},
				},
			},
		},
	}
	asyncF = function.Function{
		ID:   "test",
		Name: "test",
		Triggers: []function.Trigger{
			{
				EventTrigger: &function.EventTrigger{
					Event: "test-evt",
				},
			},
		},
		Steps: map[string]function.Step{
			"1": {
				ID: "1",
				Runtime: inngest.RuntimeWrapper{
					Runtime: &mockdriver.Mock{},
				},
				After: []function.After{
					{
						Step: inngest.TriggerName,
						Async: &inngest.AsyncEdgeMetadata{
							TTL:   timeout.String(),
							Event: "async/continue",
						},
					},
				},
			},
			"2": {
				ID: "2",
				Runtime: inngest.RuntimeWrapper{
					Runtime: &mockdriver.Mock{},
				},
				After: []function.After{
					{
						Step: inngest.TriggerName,
						Async: &inngest.AsyncEdgeMetadata{
							TTL:   timeout.String(),
							Event: "async/do-not-continue",
							// This should run, as the do-not-continue is not
							// sent and we should time out.
							OnTimeout: true,
						},
					},
				},
			},
			"3": {
				ID: "3",
				Runtime: inngest.RuntimeWrapper{
					Runtime: &mockdriver.Mock{},
				},
				After: []function.After{
					{
						Step: inngest.TriggerName,
						Async: &inngest.AsyncEdgeMetadata{
							TTL:   timeout.String(),
							Event: "async/do-not-continue",
						},
					},
				},
			},
		},
	}
)

func prepare(ctx context.Context, t *testing.T, f function.Function) prepared {
	t.Helper()

	// Create a new state manager and queue, in-memory
	c, err := config.Parse([]byte(`package main

import (
	config "inngest.com/defs/config"
)

config.#Config & {
	execution: {
		drivers: {
			http: config.#MockDriver & {
				driver: "http"
			}
			docker: config.#MockDriver & {
				driver: "docker"
			}
		}
	}
}`))

	require.NoError(t, err)
	sm, err := c.State.Service.Concrete.Manager(ctx)
	require.NoError(t, err)
	q, err := c.Queue.Service.Concrete.Queue()
	require.NoError(t, err)

	w, err := f.Workflow(ctx)
	require.NoError(t, err)

	// Create a new execution environment.
	al := &inmemorydatastore.MemoryExecutionLoader{}
	err = al.SetFunctions(ctx, []*function.Function{&f})
	require.NoError(t, err)

	return prepared{
		c:  c,
		q:  q,
		sm: sm,
		al: al,
		f:  f,
		w:  *w,
	}
}

func TestPre(t *testing.T) {
	ctx := context.Background()
	prepared := prepare(ctx, t, syncF)
	// Create a new service.
	svc := NewService(*prepared.c)
	// This should return nil
	err := svc.Pre(ctx)
	require.NoError(t, err)
}

func TestHandleQueueItemTriggerService(t *testing.T) {
	// We assume that when handling the trigger, the pending count already
	// has a pending count of 1.
	ctx := context.Background()
	data := prepare(ctx, t, syncF)
	data.c.Execution.Drivers["mock"] = &mockdriver.Config{
		Responses: map[string]state.DriverResponse{
			"1": {Output: map[string]interface{}{"id": 1}},
		},
	}
	svc := NewService(*data.c, WithExecutionLoader(data.al))

	go func() {
		err := service.Start(ctx, svc)
		require.NoError(t, err)
	}()

	// Create a new run.
	id := state.Identifier{
		WorkflowID: data.w.UUID,
		RunID:      ulid.MustNew(ulid.Now(), rand.Reader),
	}

	_, err := data.sm.New(ctx, data.w, id, (event.Event{
		Name: "test",
		Data: map[string]interface{}{
			"data": "ya",
		},
	}).Map())
	require.NoError(t, err)

	// Require that we have a pending count.
	run, err := data.sm.Load(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 1, run.Metadata().Pending)

	// Publish an entry to the queue.
	err = data.q.Enqueue(ctx, queue.Item{
		Kind:       queue.KindEdge,
		Identifier: id,
		Payload:    queue.PayloadEdge{Edge: inngest.SourceEdge},
	}, time.Now())
	require.NoError(t, err)

	// This should execute all of our items.
	<-time.After(buffer)

	run, err = data.sm.Load(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 3, len(run.Actions()))
	require.Equal(t, 0, run.Metadata().Pending)
}

// TestHandleAsync ensures correctness when hitting an async edge.  Technically,
// once we hit an async edge we need to:
//
// - Insert a Pause for the async expression
// - Increase the Pending count, as the async edge is pending until either timeout
//   or the event is received.
func TestHandleAsyncService(t *testing.T) {
	// We assume that when handling the trigger, the pending count already
	// has a pending count of 1.
	ctx := context.Background()
	data := prepare(ctx, t, asyncF)
	data.c.Execution.Drivers["mock"] = &mockdriver.Config{
		Responses: map[string]state.DriverResponse{
			"1": {Output: map[string]interface{}{"id": 1}},
			"2": {Output: map[string]interface{}{"id": 2}},
			"3": {Err: fmt.Errorf("should not run")},
		},
	}

	// Ensure that we add async expressions.

	svc := NewService(*data.c, WithExecutionLoader(data.al))
	go func() {
		err := service.Start(ctx, svc)
		require.NoError(t, err)
	}()

	// Create a new run.
	id := state.Identifier{
		WorkflowID: data.w.UUID,
		RunID:      ulid.MustNew(ulid.Now(), rand.Reader),
	}

	_, err := data.sm.New(ctx, data.w, id, (event.Event{
		Name: "test",
		Data: map[string]interface{}{
			"data": "ya",
		},
	}).Map())
	require.NoError(t, err)

	// Require that we have a pending count.
	run, err := data.sm.Load(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 1, run.Metadata().Pending)

	// Publish an entry to the queue.
	err = data.q.Enqueue(ctx, queue.Item{
		Kind:       queue.KindEdge,
		Identifier: id,
		Payload:    queue.PayloadEdge{Edge: inngest.SourceEdge},
	}, time.Now())
	require.NoError(t, err)

	// This should execute all of our items.
	<-time.After(buffer)

	// We should have only executed the trigger, with no responses saved
	// and 3 pending.
	//
	// This is because all child actions require an event.
	run, err = data.sm.Load(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 0, len(run.Actions()))
	require.Equal(t, 3, run.Metadata().Pending)

	pauses, err := data.sm.PausesByEvent(ctx, "async/continue")
	require.NoError(t, err)
	require.True(t, pauses.Next(ctx))
	pause := pauses.Val(ctx)

	// Pretend that we received an "async/continue" event via the runner.
	err = data.sm.ConsumePause(ctx, pause.ID)
	require.NoError(t, err)
	err = data.q.Enqueue(
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
	)
	require.NoError(t, err)

	<-time.After(buffer)

	// We should have exected the first pause.
	//
	// This is because all child actions require an event.
	run, err = data.sm.Load(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 1, len(run.Actions()))
	require.EqualValues(t, map[string]map[string]interface{}{
		"1": {"id": 1},
	}, run.Actions())
	require.Equal(t, 2, run.Metadata().Pending)

	// And then, after timing out of the async do not continue event,
	// our counter should be 0 and we should have only the timeout event
	// stored
	<-time.After(timeout + buffer)
	run, err = data.sm.Load(ctx, id)
	require.NoError(t, err)
	require.EqualValues(t, map[string]map[string]interface{}{
		"1": {"id": 1},
		"2": {"id": 2},
	}, run.Actions())
	require.Equal(t, 0, run.Metadata().Pending)

}
