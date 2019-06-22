package luddite

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dimfeld/httptreemux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/cors"
	log "github.com/sirupsen/logrus"
	"gopkg.in/SpirentOrion/trace.v2"
)

var (
	negotiatedContentTypes = []string{
		ContentTypeJson,
		ContentTypeCss,
		ContentTypePlain,
		ContentTypeXml,
		ContentTypeHtml,
		ContentTypeGif,
		ContentTypePng,
		ContentTypeOctetStream,
	}

	responseWriterPool = sync.Pool{New: func() interface{} { return new(responseWriter) }}
	handlerDetailsPool = sync.Pool{New: func() interface{} { return new(handlerDetails) }}
)

// Service implements a standalone RESTful web service.
type Service struct {
	config          *ServiceConfig
	defaultLogger   *log.Logger
	accessLogger    *log.Logger
	globalRouter    *httptreemux.ContextMux
	apiRouters      map[int]*httptreemux.ContextMux
	handlers        []http.Handler
	cors            *cors.Cors
	tracer          context.Context
	schemas         http.FileSystem
	once            sync.Once
	recoveryHandler func(handler func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request)
}

// NewService creates a new Service instance based on the given config.
// Middleware handlers and resources should be added before the service is run.
// The service may be run one time.
func NewService(config *ServiceConfig) (*Service, error) {
	// Normalize and validate config
	config.Normalize()
	if err := config.Validate(); err != nil {
		return nil, err
	}

	// Create the service and its routers
	s := &Service{
		config:          config,
		globalRouter:    newRouter(config.Prefix),
		apiRouters:      make(map[int]*httptreemux.ContextMux, config.Version.Max-config.Version.Min+1),
		recoveryHandler: defaultRecoveryHandler,
	}
	for v := config.Version.Min; v <= config.Version.Max; v++ {
		s.apiRouters[v] = newRouter(config.Prefix)
	}

	// Create the service loggers
	s.defaultLogger = &log.Logger{
		Formatter: new(log.JSONFormatter),
	}
	if config.Log.ServiceLogPath != "" {
		// Service log to file
		openLogFile(s.defaultLogger, config.Log.ServiceLogPath)
	} else {
		// Service log to stdout
		s.defaultLogger.Out = os.Stdout
	}

	switch strings.ToLower(config.Log.ServiceLogLevel) {
	case "debug":
		s.defaultLogger.SetLevel(log.DebugLevel)
	default:
		fallthrough
	case "info":
		s.defaultLogger.SetLevel(log.InfoLevel)
	case "warn":
		s.defaultLogger.SetLevel(log.WarnLevel)
	case "error":
		s.defaultLogger.SetLevel(log.ErrorLevel)
	}

	if config.Log.AccessLogPath != "" {
		// Access log to file
		s.accessLogger = &log.Logger{
			Formatter: new(log.JSONFormatter),
			Level:     log.InfoLevel,
		}
		openLogFile(s.accessLogger, config.Log.AccessLogPath)
	} else if config.Log.ServiceLogPath != "" {
		// Access log to stdout
		s.accessLogger = &log.Logger{
			Formatter: new(log.JSONFormatter),
			Level:     log.InfoLevel,
			Out:       os.Stdout,
		}
	} else {
		// Both service log and access log to stdout (sharing a logger)
		s.accessLogger = s.defaultLogger
	}

	// Add default middleware handlers
	s.AddHandler(newNegotiatorHandler(negotiatedContentTypes))
	s.AddHandler(newVersionHandler(s.config.Version.Min, s.config.Version.Max))

	// Create the default schema filesystem
	if config.Schema.Enabled {
		s.schemas = http.Dir(config.Schema.FilePath)
	}

	// Dump goroutine stacks on demand
	dumpGoroutineStacks()
	return s, nil
}

// Config returns the service's ServiceConfig instance.
func (s *Service) Config() *ServiceConfig {
	return s.config
}

// Logger returns the service's log.Logger instance.
func (s *Service) Logger() *log.Logger {
	return s.defaultLogger
}

// Router returns the service's router instance for the given API version.
func (s *Service) Router(version int) (*httptreemux.ContextMux, error) {
	if version < s.config.Version.Min || version > s.config.Version.Max {
		return nil, fmt.Errorf("API version is out of range (min: %d, max: %d)", s.config.Version.Min, s.config.Version.Max)
	}
	router := s.apiRouters[version]
	return router, nil
}

// AddHandler adds a middleware handler to the service's middleware stack. All
// handlers must be added before Run is called.
func (s *Service) AddHandler(h http.Handler) {
	s.handlers = append(s.handlers, h)
}

// AddResource is a convenience method that performs runtime type assertions on
// a resource handler and adds routes as appropriate based on what interfaces
// are implemented. The same effect can be achieved by calling the various
// "Add*CollectionResource" and "Add*SingletonResource" functions with the
// appropriate router instance.
func (s *Service) AddResource(version int, basePath string, r interface{}) error {
	router, err := s.Router(version)
	if err != nil {
		return err
	}

	s.addCollectionRoutes(router, basePath, r)
	s.addSingletonRoutes(router, basePath, r)
	return nil
}

// SetSchemas allows a service to provide its own HTTP filesystem to be used for
// schema assets. This overrides the use of the local filesystem and paths given
// in the service config.
func (s *Service) SetSchemas(schemas http.FileSystem) {
	s.schemas = schemas
}

// Run starts the service's HTTP server and runs it forever or until SIGINT is
// received. This method should be invoked once per service.
func (s *Service) Run() (err error) {
	s.once.Do(func() { err = s.run() })
	return
}

func (s *Service) addMetricsRoute() {
	h := prometheus.UninstrumentedHandler()
	s.globalRouter.GET(s.config.Metrics.URIPath, h.ServeHTTP)
}

func (s *Service) addProfilerRoutes() {
	router := s.globalRouter
	uriPath := s.config.Profiler.URIPath
	router.GET(path.Join(uriPath, "/"), pprof.Index)
	router.GET(path.Join(uriPath, "/*path"), pprof.Index)
	router.GET(path.Join(uriPath, "/cmdline"), pprof.Cmdline)
	router.GET(path.Join(uriPath, "/profile"), pprof.Profile)
	router.POST(path.Join(uriPath, "/profile"), pprof.Profile)
	router.GET(path.Join(uriPath, "/symbol"), pprof.Symbol)
	router.POST(path.Join(uriPath, "/symbol"), pprof.Symbol)
	router.GET(path.Join(uriPath, "/trace"), pprof.Trace)
	router.POST(path.Join(uriPath, "/trace"), pprof.Trace)
}

func (s *Service) addSchemaRoutes() {
	config := s.config
	router := s.globalRouter

	// Serve the various schemas, e.g. /schema/v1, /schema/v2, etc.
	h := newSchemaHandler(s.schemas)
	router.GET(path.Join(config.Schema.URIPath, ":version/*filepath"), h.ServeHTTP)

	// Temporarily redirect (307) the base schema path to the default schema file, e.g. /schema -> /schema/v2/fileName
	defaultSchemaPath := path.Join(config.Prefix, config.Schema.URIPath, fmt.Sprintf("v%d", config.Version.Max), config.Schema.FileName)
	router.GET(config.Schema.URIPath, func(rw http.ResponseWriter, req *http.Request) {
		http.Redirect(rw, req, defaultSchemaPath, http.StatusTemporaryRedirect)
	})

	// Temporarily redirect (307) the version schema path to the default schema file, e.g. /schema/v2 -> /schema/v2/fileName
	router.GET(path.Join(config.Schema.URIPath, ":version"), func(rw http.ResponseWriter, req *http.Request) {
		http.Redirect(rw, req, defaultSchemaPath, http.StatusTemporaryRedirect)
	})

	// Optionally temporarily redirect (307) the root to the base schema path, e.g. / -> /schema
	if config.Schema.RootRedirect {
		router.GET("/", func(rw http.ResponseWriter, req *http.Request) {
			http.Redirect(rw, req, config.Schema.URIPath, http.StatusTemporaryRedirect)
		})
	}
}

func (s *Service) addCollectionRoutes(router *httptreemux.ContextMux, basePath string, r interface{}) {
	if x, ok := r.(CollectionLister); ok {
		AddListCollectionRoute(router, basePath, x)
	}
	if x, ok := r.(CollectionCounter); ok {
		AddCountCollectionRoute(router, basePath, x)
	}
	if x, ok := r.(CollectionGetter); ok {
		AddGetCollectionRoute(router, basePath, x)
	}
	if x, ok := r.(CollectionCreator); ok {
		AddCreateCollectionRoute(router, basePath, x)
	}
	if x, ok := r.(CollectionUpdater); ok {
		AddUpdateCollectionRoute(router, basePath, x)
	}
	if x, ok := r.(CollectionDeleter); ok {
		AddDeleteCollectionRoute(router, basePath, x)
	}
	if x, ok := r.(CollectionActioner); ok {
		AddActionCollectionRoute(router, basePath, x)
	}
}

func (s *Service) addSingletonRoutes(router *httptreemux.ContextMux, basePath string, r interface{}) {
	if x, ok := r.(SingletonGetter); ok {
		AddGetSingletonRoute(router, basePath, x)
	}
	if x, ok := r.(SingletonUpdater); ok {
		AddUpdateSingletonRoute(router, basePath, x)
	}
	if x, ok := r.(SingletonActioner); ok {
		AddActionSingletonRoute(router, basePath, x)
	}
}

func (s *Service) run() error {
	config := s.config

	// Optionally enable CORS
	if config.CORS.Enabled {
		opts := cors.Options{
			AllowedOrigins:   config.CORS.AllowedOrigins,
			AllowedMethods:   config.CORS.AllowedMethods,
			AllowedHeaders:   config.CORS.AllowedHeaders,
			ExposedHeaders:   config.CORS.ExposedHeaders,
			AllowCredentials: config.CORS.AllowCredentials,
		}
		s.cors = cors.New(opts)
	}

	// Optionally enable trace recording
	if config.Trace.Enabled {
		var (
			rec trace.Recorder
			err error
		)
		if rec = recorders[config.Trace.Recorder]; rec == nil {
			// Automatically create JSON and YAML recorders if they are not otherwise registered
			switch config.Trace.Recorder {
			case "json":
				if p := config.Trace.Params["path"]; p != "" {
					var f *os.File
					if f, err = os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666); err != nil {
						break
					}
					rec = trace.NewJSONRecorder(f)
				} else {
					err = errors.New("JSON trace recorders require a 'path' parameter")
				}
			case "yaml":
				if p := config.Trace.Params["path"]; p != "" {
					var f *os.File
					if f, err = os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666); err != nil {
						break
					}
					rec = &yamlRecorder{f}
				} else {
					err = errors.New("YAML trace recorders require a 'path' parameter")
				}
			default:
				err = fmt.Errorf("unknown trace recorder: %s", config.Trace.Recorder)
			}
		}
		if rec != nil {
			ctx := trace.WithBuffer(context.Background(), config.Trace.Buffer)
			ctx = trace.WithLogger(ctx, s.defaultLogger)
			s.tracer, _ = trace.Record(ctx, rec)
		}
		if err != nil {
			s.defaultLogger.Warn("trace recording is not active: ", err)
		}
	}

	// Add optional HTTP handlers
	if s.config.Metrics.Enabled {
		s.addMetricsRoute()
	}
	if s.config.Profiler.Enabled {
		s.addProfilerRoutes()
	}
	if config.Schema.Enabled {
		s.addSchemaRoutes()
	}

	// Serve HTTP or HTTPS, depending on config. Use stoppable listener so
	// we can exit gracefully if signaled to do so.
	var (
		l   net.Listener
		err error
	)
	if config.Transport.TLS {
		s.defaultLogger.Debugf("HTTPS listening on %s", config.Addr)
		l, err = NewStoppableTLSListener(config.Addr, true, config.Transport.CertFilePath, config.Transport.KeyFilePath)
	} else {
		s.defaultLogger.Debugf("HTTP listening on %s", config.Addr)
		l, err = NewStoppableTCPListener(config.Addr, true)
	}
	if err != nil {
		return err
	}

	// If metrics are enabled let Prometheus have a look at the request first
	var h http.HandlerFunc
	if config.Metrics.Enabled {
		h = prometheus.InstrumentHandler("service", s)
	} else {
		h = s.ServeHTTP
	}

	// Run the HTTP server
	if err = http.Serve(l, h); err != nil {
		// Ignore ListenerStoppedError
		if _, ok := err.(*ListenerStoppedError); ok {
			err = nil
		}
	}
	return err
}

func (s *Service) SetRecoveryHandler(handler func(h func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request)) {
	if handler == nil {
		handler = defaultRecoveryHandler
	}
	s.recoveryHandler = handler
}

func (s *Service) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	var (
		start    = time.Now()
		traceId  int64
		parentId int64
		res      *responseWriter
		d        *handlerDetails
		err      error
	)

	// Don't allow panics to escape under any circumstances!
	defer func() {
		if rcv := recover(); rcv != nil {
			stack := make([]byte, maxStackSize)
			stack = stack[:runtime.Stack(stack, false)]
			s.defaultLogger.WithFields(log.Fields{
				"stack": string(stack),
			}).Error(rcv)
		}
		if res != nil {
			responseWriterPool.Put(res)
		}
		if d != nil {
			handlerDetailsPool.Put(d)
		}
	}()

	// Handle CORS prior to tracing
	if s.cors != nil {
		s.cors.HandlerFunc(rw, req)
		if req.Method == "OPTIONS" {
			return
		}
	}

	// If tracing is enabled then join the request and trace contexts
	ctx0 := req.Context()
	if s.tracer != nil {
		if ctx0, err = trace.Join(ctx0, s.tracer); err != nil {
			// NB: This shouldn't happen but if they do, silently
			// recover from them on the basis that tracing failures
			// shouldn't produce service failures.
			ctx0 = req.Context()
		}
	}

	// Trace using either using an existing trace id (recovered from the
	// X-Request-Id header in the form "traceId:parentId") or a newly
	// generated one. Add the trace id to the request context.
	if hdr := req.Header.Get(HeaderRequestId); hdr != "" {
		if parts := strings.Split(hdr, ":"); len(parts) == 2 {
			traceId, _ = strconv.ParseInt(parts[0], 10, 64)
			parentId, _ = strconv.ParseInt(parts[1], 10, 64)
		}
	}
	if traceId > 0 && parentId > 0 {
		ctx0 = trace.WithTraceID(trace.WithParentID(ctx0, parentId), traceId)
	} else {
		traceId, _ = trace.GenerateID(ctx0)
		ctx0 = trace.WithTraceID(ctx0, traceId)
	}
	requestId := strconv.FormatInt(traceId, 10)
	rw.Header().Set(HeaderRequestId, requestId)

	// Handle the remainder of request processing in a trace span
	trace.Do(ctx0, TraceKindRequest, req.URL.Path, func(ctx1 context.Context) {
		// Create a new response writer
		res = responseWriterPool.Get().(*responseWriter)
		res.init(rw)

		// Create new handler details and to the request context
		d = handlerDetailsPool.Get().(*handlerDetails)
		d.init(s, res, req, requestId, "luddite.ServeHTTP.begin")
		ctx1 = withHandlerDetails(ctx1, d)

		// Create a shallow copy of the request so that it references
		// the final and correct context
		req = req.WithContext(ctx1)
		d.request = req

		defer func() {
			var (
				latency = time.Since(start)
				status  = res.Status()
				rcv     interface{}
				stack   string
			)

			// If a panic occurs in a downstream handler generate a fail-safe response
			if rcv = recover(); rcv != nil {
				var resp *Error
				if err, ok := rcv.(error); ok && err == context.Canceled {
					// Context cancelation is not an error: use the 418 status as a log marker
					status = http.StatusTeapot
				} else {
					// Unhandled error: return a 500 response
					stackBuffer := make([]byte, maxStackSize)
					stack = string(stackBuffer[:runtime.Stack(stackBuffer, false)])
					s.defaultLogger.WithFields(log.Fields{"stack": stack}).Error(rcv)

					resp = NewError(nil, EcodeInternal, rcv)
					if s.config.Debug.Stacks {
						if respStackSize := s.config.Debug.StackSize; len(stack) > respStackSize {
							resp.Stack = stack[:respStackSize]
						} else {
							resp.Stack = stack
						}
					}
					status = http.StatusInternalServerError
				}
				_ = WriteResponse(res, status, resp)
			}

			// Log the request
			apiVersion := req.Header.Get(HeaderSpirentApiVersion)
			if apiVersion == "" {
				apiVersion = res.Header().Get(HeaderSpirentApiVersion)
			}
			fields := log.Fields{
				"client_addr":   req.RemoteAddr,
				"forwarded_for": req.Header.Get(HeaderForwardedFor),
				"proto":         req.Proto,
				"method":        req.Method,
				"uri":           req.RequestURI,
				"status":        status,
				"size":          res.Size(),
				"user_agent":    req.UserAgent(),
				"request_id":    requestId,
				"api_version":   apiVersion,
				"latency":       fmt.Sprintf("%.6f", latency.Seconds()),
			}
			sessionId := req.Header.Get(HeaderSessionId)
			if sessionId != "" {
				fields["session_id"] = sessionId
			}
			entry := s.accessLogger.WithFields(fields)
			if status/100 != 5 {
				entry.Info()
			} else {
				entry.Error()
			}

			// Annotate the trace
			if data := trace.Annotate(ctx1); data != nil {
				data["request_method"] = req.Method
				data["request_id"] = requestId
				data["request_progress"] = ContextRequestProgress(ctx1)
				data["response_status"] = res.Status()
				data["response_size"] = res.Size()
				if req.URL.RawQuery != "" {
					data["query"] = req.URL.RawQuery
				}
				if sessionId != "" {
					data["session_id"] = sessionId
				}
				if rcv != nil {
					data["panic"] = rcv
					data["stack"] = stack
				}
			}
		}()

		// Run the request through the service's middleware handlers. If
		// any handler generates a response then we are done.
		for _, h := range s.handlers {
			s.recoveryHandler(h.ServeHTTP)(res, req)
			if res.Written() {
				return
			}
		}

		// Try a route lookup using the global router. Routes registered
		// here have preference over API version-specific routes and are
		// served w/o regard to requested API version number.
		if lr, ok := s.globalRouter.Lookup(nil, req); ok {
			s.globalRouter.ServeLookupResult(res, req, lr)
			return
		}

		// Finally, dispatch to a resource via an API router
		router := s.apiRouters[d.apiVersion]
		s.recoveryHandler(router.ServeHTTP)(res, req)
	})
}

func newRouter(prefix string) *httptreemux.ContextMux {
	router := httptreemux.NewContextMux()
	router.NotFoundHandler = notFoundHandler
	if prefix != "" {
		router.ContextGroup = router.NewGroup(prefix)
	}
	return router
}

func notFoundHandler(rw http.ResponseWriter, _ *http.Request) {
	rw.WriteHeader(http.StatusNotFound)
}

func openLogFile(logger *log.Logger, logPath string) {
	sigs := make(chan os.Signal, 1)
	logging := make(chan bool, 1)

	go func() {
		var curLog *os.File
		for {
			// Open and begin using a new log file
			newLog, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
			if err != nil {
				panic(err)
			}

			logger.Out = newLog
			if curLog == nil {
				// First log, signal the outer goroutine that we're running
				logging <- true
			} else {
				// Follow-on log, close the current log file
				_ = curLog.Close()
			}
			curLog = newLog

			// Wait for a SIGHUP
			<-sigs
		}
	}()

	signal.Notify(sigs, syscall.SIGHUP)
	<-logging
}

// Default recovery handler - equivalent to the identity
func defaultRecoveryHandler(handler func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return handler
}
