package proxy

import (
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/metric/global"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/render"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"

	"github.com/hellofresh/kangal/pkg/backends"
	cHttp "github.com/hellofresh/kangal/pkg/core/http"
	mPkg "github.com/hellofresh/kangal/pkg/core/middleware"
	kube "github.com/hellofresh/kangal/pkg/kubernetes"
	"github.com/hellofresh/kangal/pkg/report"
	otelPrometheus "go.opentelemetry.io/otel/exporters/prometheus"
)

// Runner encapsulates all Kangal Proxy API server dependencies
type Runner struct {
	Exporter      *otelPrometheus.Exporter
	KubeClient    *kube.Client
	Logger        *zap.Logger
	StatsReporter *MetricsReporter
}

// RunServer runs Kangal proxy API
func RunServer(cfg Config, rr Runner) error {
	registry := backends.New(
		backends.WithLogger(rr.Logger),
	)

	proxyHandler := NewProxy(cfg.MaxLoadTestsRun, registry, rr.KubeClient, cfg.MaxListLimit, cfg.AllowedCustomImages)

	// Start instrumented server
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(mPkg.NewLogger(rr.Logger).Handler)
	r.Use(mPkg.NewRequestLogger().Handler)
	r.Use(mPkg.Recovery)
	r.Use(render.SetContentType(render.ContentTypeJSON))
	r.Use(OpenAPISpecCORSMiddleware(cfg.OpenAPI))

	r.Get("/status", cHttp.LivenessHandler("Kangal Proxy"))
	r.Handle("/metrics", promhttp.Handler())

	// ---------------------------------------------------------------------- //
	// LoadTest Proxy CRUD
	// ---------------------------------------------------------------------- //
	loadtestRoute := "/load-test"
	loadtestRouteWithID := fmt.Sprintf("%s/{id}", loadtestRoute)

	r.Method(http.MethodGet,
		loadtestRoute,
		otelhttp.NewHandler(http.HandlerFunc(proxyHandler.List), loadtestRoute),
	)

	r.Method(http.MethodPost,
		loadtestRoute,
		otelhttp.NewHandler(http.HandlerFunc(proxyHandler.Create), loadtestRoute),
	)

	r.Method(http.MethodGet,
		loadtestRouteWithID,
		otelhttp.NewHandler(http.HandlerFunc(proxyHandler.Get), loadtestRouteWithID),
	)

	r.Method(http.MethodDelete,
		loadtestRouteWithID,
		otelhttp.NewHandler(http.HandlerFunc(proxyHandler.Delete), loadtestRouteWithID),
	)

	// ---------------------------------------------------------------------- //
	// LoadTest API Documentation
	// ---------------------------------------------------------------------- //
	r.Get("/", OpenAPIUIHandler(cfg.OpenAPI))
	r.Get("/openapi", OpenAPISpecHandler(cfg.OpenAPI))

	r.Get("/load-test/{id}/logs", proxyHandler.GetLogs)
	r.Get("/load-test/{id}/logs/{worker}", proxyHandler.GetLogs)

	// ---------------------------------------------------------------------- //
	// LoadTest reports
	// ---------------------------------------------------------------------- //
	r.Get("/load-test/{id}/report", func(w http.ResponseWriter, r *http.Request) {
		url := fmt.Sprintf("%s/", r.URL.Host+r.URL.Path)
		http.Redirect(w, r, url, http.StatusMovedPermanently)
	})
	r.Get("/load-test/{id}/report/*", report.ShowHandler())
	r.Put("/load-test/{id}/report", report.PersistHandler(rr.KubeClient, rr.Logger))

	address := fmt.Sprintf(":%d", cfg.HTTPPort)
	rr.Logger.Info("Running HTTP server...", zap.String("address", address))

	// Try and run http server, fail on error
	err := http.ListenAndServe(address, otelhttp.NewHandler(r, "kangal", otelhttp.WithMeterProvider(global.MeterProvider()), otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents)))
	if err != nil {
		return fmt.Errorf("failed to run HTTP server: %w", err)
	}
	return nil
}
