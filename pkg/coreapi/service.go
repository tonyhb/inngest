package coreapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/inngest/inngest/pkg/config"
	"github.com/inngest/inngest/pkg/coredata"
	"github.com/inngest/inngest/pkg/logger"
	"github.com/inngest/inngest/pkg/service"
)

type Opt func(s *svc)

func NewService(c config.Config, opts ...Opt) service.Service {
	svc := &svc{config: c}
	for _, o := range opts {
		o(svc)
	}
	return svc
}

func WithAPIReadWriter(rw coredata.APIReadWriter) func(s *svc) {
	return func(s *svc) {
		s.data = rw
	}
}

type svc struct {
	config config.Config
	api    *CoreAPI
	// data provides the ability to write and load data
	data coredata.APIReadWriter
}

func (s *svc) Name() string {
	return "coreapi"
}

func (s *svc) Pre(ctx context.Context) (err error) {
	s.data, err = s.config.DataStore.Service.Concrete.ReadWriter(ctx)
	if err != nil {
		return err
	}

	// TODO - Configure API with correct ports, etc., set up routes
	s.api, err = NewCoreApi(Options{
		Config:        s.config,
		Logger:        logger.From(ctx),
		APIReadWriter: s.data,
	})

	if err != nil {
		return err
	}
	return nil
}

func (s *svc) Run(ctx context.Context) error {
	err := s.api.Start(ctx)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (s *svc) Stop(ctx context.Context) error {
	// TODO - Gracefully shut down server, remove connection to coredata database
	return s.api.Stop(ctx)
}
