package main

import (
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Foundato/kelon/common"
	"github.com/Foundato/kelon/configs"
	apiInt "github.com/Foundato/kelon/internal/pkg/api"
	"github.com/Foundato/kelon/internal/pkg/data"
	opaInt "github.com/Foundato/kelon/internal/pkg/opa"
	requestInt "github.com/Foundato/kelon/internal/pkg/request"
	translateInt "github.com/Foundato/kelon/internal/pkg/translate"
	"github.com/Foundato/kelon/internal/pkg/util"
	watcherInt "github.com/Foundato/kelon/internal/pkg/watcher"
	"github.com/Foundato/kelon/pkg/api"
	"github.com/Foundato/kelon/pkg/constants"
	"github.com/Foundato/kelon/pkg/constants/logging"
	"github.com/Foundato/kelon/pkg/opa"
	"github.com/Foundato/kelon/pkg/request"
	"github.com/Foundato/kelon/pkg/telemetry"
	"github.com/Foundato/kelon/pkg/translate"
	"github.com/Foundato/kelon/pkg/watcher"
	log "github.com/sirupsen/logrus"
	"gopkg.in/alecthomas/kingpin.v2"
)

//nolint:gochecknoglobals,gocritic
var (
	app = kingpin.New("kelon", "Kelon policy enforcer.")

	// Commands
	run = app.Command("run", "Run kelon in production mode.")

	// Config paths
	datastorePath     = app.Flag("datastore-conf", "Path to the datastore configuration yaml.").Short('d').Default("./datastore.yml").Envar("DATASTORE_CONF").ExistingFile()
	apiPath           = app.Flag("api-conf", "Path to the api configuration yaml.").Short('a').Default("./api.yml").Envar("API_CONF").ExistingFile()
	configWatcherPath = app.Flag("config-watcher-path", "Path where the config watcher should listen for changes.").Envar("CONFIG_WATCHER_PATH").ExistingDir()
	opaPath           = app.Flag("opa-conf", "Path to the OPA configuration yaml.").Short('o').Default("./opa.yml").Envar("OPA_CONF").ExistingFile()
	regoDir           = app.Flag("rego-dir", "Dir containing .rego files which will be loaded into OPA.").Short('r').Envar("REGO_DIR").ExistingDir()

	// Additional config
	pathPrefix            = app.Flag("path-prefix", "Prefix which is used to proxy OPA's Data-API.").Default("/v1").Envar("PATH_PREFIX").String()
	port                  = app.Flag("port", "Port on which the proxy endpoint is served.").Short('p').Default("8181").Envar("PORT").Uint32()
	preprocessRegos       = app.Flag("preprocess-policies", "Preprocess incoming policies for internal use-case (EXPERIMENTAL FEATURE! DO NOT USE!).").Default("false").Envar("PREPROCESS_POLICIES").Bool()
	respondWithStatusCode = app.Flag("respond-with-status-code", "Communicate Decision via status code 200 (ALLOW) or 403 (DENY).").Default("false").Envar("RESPOND_WITH_STATUS_CODE").Bool()

	// Logging
	logLevel               = app.Flag("log-level", "Log-Level for Kelon. Must be one of [DEBUG, INFO, WARN, ERROR]").Default("INFO").Envar("LOG_LEVEL").Enum("DEBUG", "INFO", "WARN", "ERROR", "debug", "info", "warn", "error")
	logFormat              = app.Flag("log-format", "Log-Format for Kelon. Must be one of [TEXT, JSON]").Default("TEXT").Envar("LOG_FORMAT").Enum("TEXT", "JSON")
	accessDecisionLogLevel = app.Flag("access-decision-log-level", "Access decision Log-Level for Kelon. Must be one of [ALL, ALLOW, DENY, NONE]").Default("ALL").Envar("ACCESS_DECISION_LOG_LEVEL").Enum("ALL", "ALLOW", "DENY", "NONE", "all", "allow", "deny", "none")

	// Configs for telemetry
	telemetryService = app.Flag("telemetry-service", "Service that is used for telemetry [Prometheus]").Envar("TELEMETRY_SERVICE").Enum("Prometheus", "prometheus")

	// Global shared variables
	proxy             api.ClientProxy       = nil
	configWatcher     watcher.ConfigWatcher = nil
	telemetryProvider telemetry.Provider    = nil
)

func main() {
	app.HelpFlag.Short('h')
	app.Version(common.Version)

	// Process args and initialize logger
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	case run.FullCommand():
		log.SetOutput(os.Stdout)

		// Set log format
		switch *logFormat {
		case "JSON":
			log.SetFormatter(util.UTCFormatter{Formatter: &log.JSONFormatter{}})
		default:
			log.SetFormatter(util.UTCFormatter{Formatter: &log.TextFormatter{FullTimestamp: true}})
		}

		// Set log level
		switch strings.ToUpper(*logLevel) {
		case "INFO":
			log.SetLevel(log.InfoLevel)
		case "DEBUG":
			log.SetLevel(log.DebugLevel)
		case "WARN":
			log.SetLevel(log.WarnLevel)
		case "ERROR":
			log.SetLevel(log.ErrorLevel)
		}
		logging.LogForComponent("main").Infof("Kelon starting with log level %q...", *logLevel)

		// Init config loader
		configLoader := configs.FileConfigLoader{
			DatastoreConfigPath: *datastorePath,
			APIConfigPath:       *apiPath,
		}

		// Start app after config is present
		makeConfigWatcher(configLoader, configWatcherPath)
		configWatcher.Watch(onConfigLoaded)
		stopOnSIGTERM()
	default:
		logging.LogForComponent("main").Fatal("Started Kelon with a unknown command!")
	}
}

func onConfigLoaded(change watcher.ChangeType, loadedConf *configs.ExternalConfig, err error) {
	if err != nil {
		logging.LogForComponent("main").Fatalln("Unable to parse configuration: ", err.Error())
	}

	if change == watcher.ChangeAll {
		// Configure application
		var (
			config     = new(configs.AppConfig)
			compiler   = opaInt.NewPolicyCompiler()
			parser     = requestInt.NewURLProcessor()
			mapper     = requestInt.NewPathMapper()
			translator = translateInt.NewAstTranslator()
		)

		// Build config
		config.API = loadedConf.API
		config.Data = loadedConf.Data
		config.TelemetryProvider = makeTelemetryProvider()
		telemetryProvider = config.TelemetryProvider // Stopped gracefully later on
		serverConf := makeServerConfig(compiler, parser, mapper, translator, loadedConf)

		if *preprocessRegos {
			*regoDir = util.PrepocessPoliciesInDir(config, *regoDir)
		}

		// Start rest proxy
		startNewRestProxy(config, &serverConf)
	}
}

func makeTelemetryProvider() telemetry.Provider {
	var provider telemetry.Provider
	if telemetryService != nil {
		if strings.EqualFold(*telemetryService, constants.PrometheusTelemetry) {
			provider = &telemetry.Prometheus{}
		}

		if provider != nil {
			if err := provider.Configure(); err != nil {
				logging.LogForComponent("main").Fatalf("Error during configuration of TelemetryProvider %q: %s", *telemetryService, err.Error())
			}
		}
	}
	return provider
}

func makeConfigWatcher(configLoader configs.FileConfigLoader, configWatcherPath *string) {
	if regoDir == nil || *regoDir == "" {
		configWatcher = watcherInt.NewSimple(configLoader)
	} else {
		// Set configWatcherPath to rego path by default
		if configWatcherPath == nil || *configWatcherPath == "" {
			configWatcherPath = regoDir
		}
		configWatcher = watcherInt.NewFileWatcher(configLoader, *configWatcherPath)
	}
}

func startNewRestProxy(appConfig *configs.AppConfig, serverConf *api.ClientProxyConfig) {
	// Create Rest proxy and start
	proxy = apiInt.NewRestProxy(*pathPrefix, int32(*port))
	if err := proxy.Configure(appConfig, serverConf); err != nil {
		logging.LogForComponent("main").Fatalln(err.Error())
	}
	// Start proxy
	if err := proxy.Start(); err != nil {
		logging.LogForComponent("main").Fatalln(err.Error())
	}
}

func makeServerConfig(compiler opa.PolicyCompiler, parser request.PathProcessor, mapper request.PathMapper, translator translate.AstTranslator, loadedConf *configs.ExternalConfig) api.ClientProxyConfig {
	// Build server config
	serverConf := api.ClientProxyConfig{
		Compiler: &compiler,
		PolicyCompilerConfig: opa.PolicyCompilerConfig{
			RespondWithStatusCode: *respondWithStatusCode,
			Prefix:                pathPrefix,
			OpaConfigPath:         opaPath,
			RegoDir:               regoDir,
			ConfigWatcher:         &configWatcher,
			PathProcessor:         &parser,
			PathProcessorConfig: request.PathProcessorConfig{
				PathMapper: &mapper,
			},
			Translator: &translator,
			AstTranslatorConfig: translate.AstTranslatorConfig{
				Datastores: data.MakeDatastores(loadedConf.Data),
			},
			AccessDecisionLogLevel: strings.ToUpper(*accessDecisionLogLevel),
		},
	}
	return serverConf
}

func stopOnSIGTERM() {
	interruptChan := make(chan os.Signal, 1)
	signal.Notify(interruptChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	// Block until we receive our signal.
	<-interruptChan

	logging.LogForComponent("main").Infoln("Caught SIGTERM...")
	// Stop telemetry provider if present
	// This is done blocking to ensure all telemetries are sent!
	if telemetryProvider != nil {
		telemetryProvider.Shutdown()
	}

	// Stop rest proxy if started
	if proxy != nil {
		if err := proxy.Stop(time.Second * 10); err != nil {
			logging.LogForComponent("main").Warnln(err.Error())
		}
	}
	// Give components enough time for graceful shutdown
	// This terminates earlier, because rest-proxy prints FATAL if http-server is closed
	time.Sleep(5 * time.Second)
}
