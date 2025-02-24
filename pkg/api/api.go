package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/inngest/inngest/pkg/config"
	"github.com/inngest/inngest/pkg/event"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
)

type EventHandler func(context.Context, *event.Event) error

type Options struct {
	Config config.Config

	EventHandler EventHandler
	Logger       *zerolog.Logger
}

const (
	// DefaultMaxSize represents the maximum size of the event payload we process,
	// currently 256KB.
	DefaultMaxSize = 256 * 1024
)

var (
	EventPathRegex = regexp.MustCompile("^/e/([a-zA-Z0-9-_]+)$")
)

func NewAPI(o Options) (*API, error) {
	logger := o.Logger.With().Str("caller", "api").Logger()

	if o.Config.EventAPI.MaxSize == 0 {
		o.Config.EventAPI.MaxSize = DefaultMaxSize
	}

	api := &API{
		config:  o.Config,
		handler: o.EventHandler,
		log:     &logger,
	}

	http.HandleFunc("/", api.HealthCheck)
	http.HandleFunc("/health", api.HealthCheck)
	http.HandleFunc("/e/", api.ReceiveEvent)

	return api, nil
}

type API struct {
	config config.Config

	handler EventHandler
	log     *zerolog.Logger

	server *http.Server
}

func (a *API) Start(ctx context.Context) error {
	a.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", a.config.EventAPI.Addr, a.config.EventAPI.Port),
		Handler: http.DefaultServeMux,
	}
	a.log.Info().Str("addr", a.server.Addr).Msg("starting server")
	return a.server.ListenAndServe()
}

func (a API) Stop(ctx context.Context) error {
	return a.server.Shutdown(ctx)
}

func (a API) HealthCheck(w http.ResponseWriter, r *http.Request) {
	a.log.Trace().Msg("healthcheck")
	a.writeResponse(w, apiResponse{
		StatusCode: http.StatusOK,
		Message:    "OK",
	})
}

func (a API) ReceiveEvent(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	if r.ContentLength > int64(a.config.EventAPI.MaxSize) {
		a.writeResponse(w, apiResponse{
			StatusCode: http.StatusRequestEntityTooLarge,
			Error:      "Payload larger than maximum allowed",
		})
		return
	}

	matches := EventPathRegex.FindStringSubmatch(r.URL.Path)
	if matches == nil || len(matches) != 2 {
		a.writeResponse(w, apiResponse{
			StatusCode: http.StatusUnauthorized,
			Error:      "API Key is required",
		})
		return
	}

	// TODO: Implement key matching from core data loader.

	body, err := io.ReadAll(io.LimitReader(r.Body, int64(a.config.EventAPI.MaxSize)))
	if err != nil {
		a.writeResponse(w, apiResponse{
			StatusCode: http.StatusBadRequest,
			Error:      "Could not read event payload",
		})
		return
	}

	events, err := parseBody(body)
	if err != nil {
		a.writeResponse(w, apiResponse{
			StatusCode: http.StatusBadRequest,
			Error:      "Unable to process event payload",
		})
		return
	}

	eg := &errgroup.Group{}
	for _, evt := range events {
		copied := evt
		eg.Go(func() error {
			if err := a.handler(r.Context(), copied); err != nil {
				a.log.Error().Str("event", copied.Name).Err(err).Msg("error handling event")
				return err
			}
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		a.writeResponse(w, apiResponse{
			StatusCode: http.StatusBadRequest,
			Error:      err.Error(),
		})
	}

	a.writeResponse(w, apiResponse{
		StatusCode: http.StatusOK,
		Message:    fmt.Sprintf("Received %d events", len(events)),
	})
}
