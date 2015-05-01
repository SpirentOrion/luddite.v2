package luddite

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/SpirentOrion/httprouter"
	"github.com/quipo/statsd"
	"golang.org/x/net/context"
)

const (
	DEFAULT_STATSD_SERVER   = "127.0.0.1:8125"
	DEFAULT_STATSD_PREFIX   = "%HOST%."
	DEFAULT_STATSD_INTERVAL = 2 * time.Second
)

var (
	nullStats = &NullStats{}
)

// Service is an interface that implements a standalone RESTful web service.
type Service interface {
	// AddHandler adds a context-aware middleware handler to the
	// middleware stack. All handlers must be added before Run is
	// called.
	AddHandler(h Handler)

	// AddSingletonResource registers a singleton-style resource
	// (supporting GET and PUT methods only).
	AddSingletonResource(itemPath string, r Resource)

	// AddCollectionResource registers a collection-style resource
	// (supporting GET, POST, PUT, and DELETE methods).
	AddCollectionResource(basePath string, r Resource)

	// Config returns the service's ServiceConfig instance.
	Config() *ServiceConfig

	// Logger returns the service's log.Logger instance.
	Logger() *log.Logger

	// Router returns the service's httprouter.Router instance.
	Router() *httprouter.Router

	// Stats returns the service's Stats instance.
	Stats() Stats

	// Run is a convenience function that runs the service as an
	// HTTP server. The address is taken from the ServiceConfig
	// passed to NewService.
	Run() error
}

type service struct {
	config       *ServiceConfig
	logger       *log.Logger
	router       *httprouter.Router
	statsdClient *statsd.StatsdClient
	stats        *statsd.StatsdBuffer
	handlers     []Handler
	middleware   *middleware
	schema       *SchemaHandler
}

// Verify that service implements Service.
var _ Service = &service{}

func NewService(config *ServiceConfig) (Service, error) {
	// Create service
	s := &service{
		config: config,
		logger: log.New(os.Stderr, config.Log.Prefix, log.LstdFlags),
		router: httprouter.New(),
	}

	s.router.NotFound = func(_ context.Context, rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusNotFound)
	}

	s.router.MethodNotAllowed = func(_ context.Context, rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}

	// Create the statsd client
	if s.config.Metrics.Enabled {
		if s.config.Metrics.Server == "" {
			s.config.Metrics.Server = DEFAULT_STATSD_SERVER
		}
		if s.config.Metrics.Prefix == "" {
			s.config.Metrics.Prefix = DEFAULT_STATSD_PREFIX
		}
		if s.config.Metrics.Interval == 0 {
			s.config.Metrics.Interval = DEFAULT_STATSD_INTERVAL
		}

		s.statsdClient = statsd.NewStatsdClient(s.config.Metrics.Server, s.config.Metrics.Prefix)
		s.stats = statsd.NewStatsdBuffer(s.config.Metrics.Interval, s.statsdClient)
		s.stats.Logger = s.logger
	}

	// Create default middleware handlers
	// NB: failures to initialize/configure tracing should not fail the service startup
	bottom, err := s.newBottomHandler()
	if err != nil {
		return nil, err
	}

	negotiator, err := s.newNegotiatorHandler()
	if err != nil {
		return nil, err
	}

	context, err := s.newContextHandler()
	if err != nil {
		return nil, err
	}

	// Build middleware stack
	s.handlers = []Handler{bottom, negotiator, context}
	s.middleware = buildMiddleware(s.handlers)

	// Install default http handlers
	if config.Schema.Enabled {
		s.addSchemaRoutes()
	}

	return s, nil
}

func (s *service) AddHandler(h Handler) {
	s.handlers = append(s.handlers, h)
	s.middleware = buildMiddleware(s.handlers)
}

func (s *service) AddSingletonResource(basePath string, r Resource) {
	// GET /basePath
	addGetRoute(s.router, basePath, false, r)

	// PUT /basePath
	addUpdateRoute(s.router, basePath, false, r)

	// POST /basePath/{action}
	addActionRoute(s.router, basePath, false, r)
}

func (s *service) AddCollectionResource(basePath string, r Resource) {
	// GET /basePath
	addListRoute(s.router, basePath, r)

	// GET /basePath/{id}
	addGetRoute(s.router, basePath, true, r)

	// POST /basePath
	addCreateRoute(s.router, basePath, r)

	// PUT /basePath/{id}
	addUpdateRoute(s.router, basePath, true, r)

	// DELETE /basePath
	addDeleteRoute(s.router, basePath, false, r)

	// DELETE /basePath/{id}
	addDeleteRoute(s.router, basePath, true, r)

	// POST /basePath/{id}/{action}
	addActionRoute(s.router, basePath, true, r)
}

func (s *service) Config() *ServiceConfig {
	return s.config
}

func (s *service) Logger() *log.Logger {
	return s.logger
}

func (s *service) Router() *httprouter.Router {
	return s.router
}

func (s *service) Stats() Stats {
	if s.stats != nil {
		return s.stats
	} else {
		return nullStats
	}
}

func (s *service) Run() error {
	// Add the router as the final middleware handler
	h, err := s.newRouterHandler()
	if err != nil {
		return err
	}
	s.AddHandler(h)

	// Arrange for shutdown of the buffered statsd client
	if s.stats != nil {
		defer s.stats.Close()
	}

	// Serve HTTP or HTTPS, depending on config
	if s.config.Transport.TLS {
		s.logger.Printf("HTTPS listening on %s", s.config.Addr)
		return http.ListenAndServeTLS(s.config.Addr, s.config.Transport.CertFilePath, s.config.Transport.KeyFilePath, s.middleware)
	} else {
		s.logger.Printf("HTTP listening on %s", s.config.Addr)
		return http.ListenAndServe(s.config.Addr, s.middleware)
	}
}

func (s *service) newBottomHandler() (Handler, error) {
	return NewBottom(s.config, s.logger, s.Stats()), nil
}

func (s *service) newNegotiatorHandler() (Handler, error) {
	return NewNegotiator([]string{ContentTypeJson, ContentTypeXml, ContentTypeHtml}), nil
}

func (s *service) newContextHandler() (Handler, error) {
	if s.config.Version.Min < 1 {
		return nil, errors.New("service's minimum API version must be greater than zero")
	}
	if s.config.Version.Max < 1 {
		return nil, errors.New("service's maximum API version must be greater than zero")
	}

	return NewContext(s, s.config.Version.Min, s.config.Version.Max), nil
}

func (s *service) newRouterHandler() (Handler, error) {
	return HandlerFunc(func(ctx context.Context, rw http.ResponseWriter, r *http.Request, _ ContextHandlerFunc) {
		// No more middleware handlers: further dispatch happens via httprouter
		s.router.HandleHTTP(ctx, rw, r)
	}), nil
}

func (s *service) addSchemaRoutes() {
	config := s.config

	// Serve the various schemas, e.g. /schema/v1, /schema/v2, etc.
	s.schema = NewSchemaHandler(config.Schema.FilePath, config.Schema.FilePattern)
	s.router.GET(path.Join(config.Schema.UriPath, "/v:version"), s.schema.ServeHTTP)

	// Temporarily redirect (307) the base schema path to the default schema, e.g. /schema -> /schema/v2
	defaultSchemaPath := path.Join(config.Schema.UriPath, fmt.Sprintf("v%d", config.Version.Max))
	s.router.GET(config.Schema.UriPath, func(_ context.Context, rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, defaultSchemaPath, http.StatusTemporaryRedirect)
	})

	// Optionally temporarily redirect (307) the root to the base schema path, e.g. / -> /schema
	if config.Schema.RootRedirect {
		s.router.GET("/", func(_ context.Context, rw http.ResponseWriter, r *http.Request) {
			http.Redirect(rw, r, config.Schema.UriPath, http.StatusTemporaryRedirect)
		})
	}
}
