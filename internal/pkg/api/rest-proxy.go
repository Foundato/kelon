package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Foundato/kelon/pkg/api"

	"github.com/Foundato/kelon/configs"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
)

type restProxy struct {
	pathPrefix string
	port       int32
	configured bool
	appConf    *configs.AppConfig
	config     *api.ClientProxyConfig
	router     *mux.Router
	server     *http.Server
}

// Implements api.ClientProxy by providing OPA's Data-REST-API.
func NewRestProxy(pathPrefix string, port int32) api.ClientProxy {
	return &restProxy{
		pathPrefix: pathPrefix,
		port:       port,
		configured: false,
		appConf:    nil,
		config:     nil,
		router:     mux.NewRouter(),
	}
}

// See Configure() of api.ClientProxy
func (proxy *restProxy) Configure(appConf *configs.AppConfig, serverConf *api.ClientProxyConfig) error {
	// Configure subcomponents
	if serverConf.Compiler == nil {
		return errors.New("RestProxy: Compiler not configured! ")
	}
	compiler := *serverConf.Compiler
	if err := compiler.Configure(appConf, &serverConf.PolicyCompilerConfig); err != nil {
		return err
	}

	// Assign variables
	proxy.appConf = appConf
	proxy.config = serverConf
	proxy.configured = true
	log.Infoln("Configured RestProxy")
	return nil
}

// See Start() of api.ClientProxy
func (proxy *restProxy) Start() error {
	if !proxy.configured {
		return errors.New("RestProxy was not configured! Please call Configure(). ")
	}

	// Create Server and Route Handlers
	proxy.router.PathPrefix(proxy.pathPrefix + "/data").HandlerFunc(proxy.handleV1DataGet).Methods("GET")
	proxy.router.PathPrefix(proxy.pathPrefix + "/data").HandlerFunc(proxy.handleV1DataPost).Methods("POST")
	proxy.router.PathPrefix(proxy.pathPrefix + "/data").HandlerFunc(proxy.handleV1DataPut).Methods("PUT")
	proxy.router.PathPrefix(proxy.pathPrefix + "/data").HandlerFunc(proxy.handleV1DataDelete).Methods("DELETE")
	proxy.router.PathPrefix(proxy.pathPrefix + "/policies").HandlerFunc(proxy.handleV1PolicyPut).Methods("PUT")
	proxy.router.PathPrefix(proxy.pathPrefix + "/policies").HandlerFunc(proxy.handleV1PolicyDelete).Methods("DELETE")
	proxy.router.PathPrefix("/metrics").Handler(promhttp.Handler())

	proxy.server = &http.Server{
		Handler:      proxy.router,
		Addr:         fmt.Sprintf(":%d", proxy.port),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Start Server
	go func() {
		log.Infof("Starting server at: http://localhost:%d%s", proxy.port, proxy.pathPrefix)
		if err := proxy.server.ListenAndServe(); err != nil {
			log.Fatal(err)
		}
	}()
	return nil
}

// See Stop() of api.ClientProxy
func (proxy *restProxy) Stop(deadline time.Duration) error {
	if proxy.server == nil {
		return errors.New("RestProxy has not bin started yet")
	}
	log.Infof("Stopping server at: http://localhost:%d%s", proxy.port, proxy.pathPrefix)
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	if err := proxy.server.Shutdown(ctx); err != nil {
		return errors.Wrap(err, "Error while shutting down server")
	}
	return nil
}
