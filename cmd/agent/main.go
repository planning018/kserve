package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/kelseyhightower/envconfig"
	"github.com/kserve/kserve/pkg/agent"
	"github.com/kserve/kserve/pkg/agent/storage"
	"github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	"github.com/kserve/kserve/pkg/batcher"
	kfslogger "github.com/kserve/kserve/pkg/logger"
	"github.com/pkg/errors"
	flag "github.com/spf13/pflag"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	network "knative.dev/networking/pkg"
	pkglogging "knative.dev/pkg/logging"
	pkgnet "knative.dev/pkg/network"
	"knative.dev/pkg/signals"
	"knative.dev/serving/pkg/queue"
	"knative.dev/serving/pkg/queue/health"
	"knative.dev/serving/pkg/queue/readiness"
)

var (
	port          = flag.String("port", "9081", "Agent port")
	componentPort = flag.String("component-port", "8080", "Component port")
	// model puller flags
	enablePuller = flag.Bool("enable-puller", false, "Enable model puller")
	configDir    = flag.String("config-dir", "/mnt/configs", "directory for model config files")
	modelDir     = flag.String("model-dir", "/mnt/models", "directory for model files")
	// logger flags
	logUrl           = flag.String("log-url", "", "The URL to send request/response logs to")
	workers          = flag.Int("workers", 5, "Number of workers")
	sourceUri        = flag.String("source-uri", "", "The source URI to use when publishing cloudevents")
	logMode          = flag.String("log-mode", string(v1beta1.LogAll), "Whether to log 'request', 'response' or 'all'")
	inferenceService = flag.String("inference-service", "", "The InferenceService name to add as header to log events")
	namespace        = flag.String("namespace", "", "The namespace to add as header to log events")
	endpoint         = flag.String("endpoint", "", "The endpoint name to add as header to log events")
	component        = flag.String("component", "", "The component name (predictor, explainer, transformer) to add as header to log events")
	// batcher flags
	enableBatcher = flag.Bool("enable-batcher", false, "Enable request batcher")
	maxBatchSize  = flag.String("max-batchsize", "32", "Max Batch Size")
	maxLatency    = flag.String("max-latency", "5000", "Max Latency in milliseconds")
	// probing flags
	readinessProbeTimeout = flag.Duration("probe-period", -1, "run readiness probe with given timeout")
	// This creates an abstract socket instead of an actual file.
	unixSocketPath = "@/kserve/agent.sock"
)

type config struct {
	//Making the below fields optional since raw deployment wont have them
	ContainerConcurrency   int    `split_words:"true"`
	QueueServingPort       int    `split_words:"true"`
	UserPort               int    `split_words:"true"`
	RevisionTimeoutSeconds int    `split_words:"true"`
	ServingReadinessProbe  string `split_words:"true" required:"true"`
	// Logging configuration
	ServingLoggingConfig         string `split_words:"true"`
	ServingLoggingLevel          string `split_words:"true"`
	ServingRequestLogTemplate    string `split_words:"true"` // optional
	ServingEnableRequestLog      bool   `split_words:"true"` // optional
	ServingEnableProbeRequestLog bool   `split_words:"true"` // optional
}

type loggerArgs struct {
	loggerType       v1beta1.LoggerType
	logUrl           *url.URL
	sourceUrl        *url.URL
	inferenceService string
	namespace        string
	endpoint         string
	component        string
}

type batcherArgs struct {
	maxBatchSize int
	maxLatency   int
}

func main() {
	flag.Parse()
	// Parse the environment.
	var env config
	if err := envconfig.Process("", &env); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// If this is set, we run as a standalone binary to probe the queue-proxy.
	if *readinessProbeTimeout >= 0 {
		// Use a unix socket rather than TCP to avoid going via entire TCP stack
		// when we're actually in the same container.
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", unixSocketPath)
		}

		os.Exit(standaloneProbeMain(*readinessProbeTimeout, transport, *port))
	}

	logger, _ := pkglogging.NewLogger(env.ServingLoggingConfig, env.ServingLoggingLevel)
	// Setup probe to run for checking user container healthiness.
	probe := buildProbe(logger, env.ServingReadinessProbe, *componentPort)

	// Poll user container health to make sure agent start after user container is ready
	logger.Infof("Probing user container with probe %v", probe)
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := wait.PollImmediateUntil(10*time.Second, func() (bool, error) {
		return probe.ProbeContainer(), nil
	}, timeoutCtx.Done()); err != nil {
		logger.Errorf("Failed to probe user container with error %v", err)
		os.Exit(1)
	}

	if *enablePuller {
		logger.Infof("Initializing model agent with config-dir %s, model-dir %s", *configDir, *modelDir)
		startModelPuller(logger)
	}

	var loggerArgs *loggerArgs
	if *logUrl != "" {
		logger.Info("Starting logger")
		loggerArgs = startLogger(*workers, logger)
	}

	var batcherArgs *batcherArgs
	if *enableBatcher {
		logger.Info("Starting batcher")
		batcherArgs = startBatcher(logger)
	}
	logger.Info("Starting agent http server...")
	healthState := &health.State{}
	ctx := signals.NewContext()
	mainServer := buildServer(ctx, *port, *componentPort, loggerArgs, batcherArgs, healthState, probe, logger)
	servers := map[string]*http.Server{
		"main": mainServer,
	}
	errCh := make(chan error)
	listenCh := make(chan struct{})
	for name, server := range servers {
		go func(name string, s *http.Server) {
			l, err := net.Listen("tcp", s.Addr)
			if err != nil {
				errCh <- fmt.Errorf("%s server failed to listen: %w", name, err)
				return
			}

			// Notify the unix socket setup that the tcp socket for the main server is ready.
			if s == mainServer {
				close(listenCh)
			}

			// Don't forward ErrServerClosed as that indicates we're already shutting down.
			if err := s.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s server failed to serve: %w", name, err)
			}
		}(name, server)
	}

	// Listen on a unix socket so that the exec probe can avoid having to go
	// through the full tcp network stack.
	go func() {
		// Only start listening on the unix socket once the tcp socket for the
		// main server is setup.
		// This avoids the unix socket path succeeding before the tcp socket path
		// is actually working and thus it avoids a race.
		<-listenCh

		l, err := net.Listen("unix", unixSocketPath)
		if err != nil {
			errCh <- fmt.Errorf("failed to listen to unix socket: %w", err)
			return
		}
		if err := http.Serve(l, mainServer.Handler); err != nil {
			errCh <- fmt.Errorf("serving failed on unix socket: %w", err)
		}
	}()

	// Blocks until we actually receive a TERM signal or one of the servers
	// exit unexpectedly. We fold both signals together because we only want
	// to act on the first of those to reach here.
	select {
	case err := <-errCh:
		logger.Errorw("Failed to bring up agent, shutting down.", zap.Error(err))
		// This extra flush is needed because defers are not handled via os.Exit calls.
		logger.Sync()
		os.Stdout.Sync()
		os.Stderr.Sync()
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("Received TERM signal, attempting to gracefully shutdown servers.")
		healthState.Shutdown(func() {
			logger.Infof("Sleeping %v to allow K8s propagation of non-ready state", pkgnet.DefaultDrainTimeout)
			time.Sleep(pkgnet.DefaultDrainTimeout)

			// Calling server.Shutdown() allows pending requests to
			// complete, while no new work is accepted.
			logger.Info("Shutting down main server")
			if err := mainServer.Shutdown(context.Background()); err != nil {
				logger.Errorw("Failed to shutdown proxy server", zap.Error(err))
			}
			// Removing the main server from the shutdown logic as we've already shut it down.
			delete(servers, "main")
		})

		for serverName, srv := range servers {
			logger.Info("Shutting down server: ", serverName)
			if err := srv.Shutdown(context.Background()); err != nil {
				logger.Errorw("Failed to shutdown server", zap.String("server", serverName), zap.Error(err))
			}
		}
		logger.Info("Shutdown complete, exiting...")
	}
}

func startBatcher(logger *zap.SugaredLogger) *batcherArgs {
	maxBatchSizeInt, err := strconv.Atoi(*maxBatchSize)
	if err != nil || maxBatchSizeInt <= 0 {
		logger.Error(errors.New("Invalid max batch size"), *maxBatchSize)
		os.Exit(1)
	}

	maxLatencyInt, err := strconv.Atoi(*maxLatency)
	if err != nil || maxLatencyInt <= 0 {
		logger.Error(errors.New("Invalid max latency"), *maxLatency)
		os.Exit(1)
	}

	return &batcherArgs{
		maxLatency:   maxLatencyInt,
		maxBatchSize: maxBatchSizeInt,
	}
}

func startLogger(workers int, logger *zap.SugaredLogger) *loggerArgs {
	loggingMode := v1beta1.LoggerType(*logMode)
	switch loggingMode {
	case v1beta1.LogAll, v1beta1.LogRequest, v1beta1.LogResponse:
	default:
		logger.Errorf("Malformed log-mode %s", *logMode)
		os.Exit(-1)
	}

	logUrlParsed, err := url.Parse(*logUrl)
	if err != nil {
		logger.Errorf("Malformed log-url %s", *logUrl)
		os.Exit(-1)
	}

	if *sourceUri == "" {
		*sourceUri = fmt.Sprintf("http://localhost:%s/", *port)
	}

	sourceUriParsed, err := url.Parse(*sourceUri)
	if err != nil {
		logger.Errorf("Malformed source_uri %s", *sourceUri)
		os.Exit(-1)
	}
	logger.Info("Starting the log dispatcher")
	kfslogger.StartDispatcher(workers, logger)
	return &loggerArgs{
		loggerType:       loggingMode,
		logUrl:           logUrlParsed,
		sourceUrl:        sourceUriParsed,
		inferenceService: *inferenceService,
		endpoint:         *endpoint,
		namespace:        *namespace,
		component:        *component,
	}
}

func startModelPuller(logger *zap.SugaredLogger) {
	downloader := agent.Downloader{
		ModelDir:  *modelDir,
		Providers: map[storage.Protocol]storage.Provider{},
		Logger:    logger,
	}
	watcher := agent.NewWatcher(*configDir, *modelDir, logger)
	logger.Info("Starting puller")
	agent.StartPullerAndProcessModels(&downloader, watcher.ModelEvents, logger)
	go watcher.Start()
}

func buildProbe(logger *zap.SugaredLogger, probeJSON string, port string) *readiness.Probe {
	coreProbe, err := readiness.DecodeProbe(probeJSON)
	if err != nil {
		logger.Fatalw("Agent failed to parse readiness probe", zap.Error(err))
	}
	if coreProbe.TCPSocket != nil {
		coreProbe.TCPSocket.Port = intstr.FromString(port)
	} else if coreProbe.HTTPGet != nil {
		coreProbe.HTTPGet.Port = intstr.FromString(port)
	}
	return readiness.NewProbe(coreProbe)
}

func buildServer(ctx context.Context, port string, userPort string, loggerArgs *loggerArgs, batcherArgs *batcherArgs,
	healthState *health.State, rp *readiness.Probe,
	logging *zap.SugaredLogger) *http.Server {
	target := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort("127.0.0.1", userPort),
	}

	maxIdleConns := 1000 // TODO: somewhat arbitrary value for CC=0, needs experimental validation.

	httpProxy := httputil.NewSingleHostReverseProxy(target)
	httpProxy.Transport = pkgnet.NewAutoTransport(maxIdleConns /* max-idle */, maxIdleConns /* max-idle-per-host */)
	httpProxy.ErrorHandler = pkgnet.ErrorHandler(logging)
	httpProxy.BufferPool = network.NewBufferPool()
	httpProxy.FlushInterval = network.FlushInterval

	// Create handler chain.
	// Note: innermost handlers are specified first, ie. the last handler in the chain will be executed first.
	var composedHandler http.Handler = httpProxy

	if batcherArgs != nil {
		composedHandler = batcher.New(batcherArgs.maxBatchSize, batcherArgs.maxLatency, composedHandler, logging)
	}
	if loggerArgs != nil {
		composedHandler = kfslogger.New(loggerArgs.logUrl, loggerArgs.sourceUrl, loggerArgs.loggerType,
			loggerArgs.inferenceService, loggerArgs.namespace, loggerArgs.endpoint, loggerArgs.component, composedHandler)
	}

	composedHandler = queue.ForwardedShimHandler(composedHandler)

	composedHandler = ProbeHandler(healthState, rp.ProbeContainer, rp.IsAggressive(), false, composedHandler)
	composedHandler = network.NewProbeHandler(composedHandler)

	return pkgnet.NewServer(":"+port, composedHandler)
}
