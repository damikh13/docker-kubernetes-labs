package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ---------- Prometheus metrics (RED) ----------

var (
	// Rate
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests.",
	}, []string{"method", "path", "status"})

	// Errors
	errorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_errors_total",
		Help: "Total number of HTTP requests that ended with a 5xx status.",
	}, []string{"method", "path"})

	// Duration
	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 1.5, 2, 2.5, 3, 4, 5, 10},
	}, []string{"method", "path"})
)

// ---------- Tracing ----------

var tracer = otel.Tracer("lab-api")

// initTracing wires up the OTLP/HTTP exporter. The endpoint is read from
// OTEL_EXPORTER_OTLP_ENDPOINT (e.g. http://jaeger:4318), the service name
// from OTEL_SERVICE_NAME.
func initTracing(ctx context.Context) (func(context.Context) error, error) {
	if os.Getenv("OTEL_SERVICE_NAME") == "" {
		os.Setenv("OTEL_SERVICE_NAME", "lab-api")
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithTelemetrySDK())
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// ---------- Logging ----------

// logFrom returns a JSON logger that already carries trace_id / span_id
// of the span stored in ctx, so every log line can be matched to a trace.
func logFrom(ctx context.Context) *slog.Logger {
	l := slog.Default()
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		l = l.With("trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
	}
	return l
}

// ---------- Middleware ----------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// instrument records RED metrics and writes one structured log line per request.
// `route` is a fixed label (not the raw URL) to keep metric cardinality low.
func instrument(route string, next http.HandlerFunc) http.Handler {
	// make the error series exist (value 0) before the first error happens
	errorsTotal.WithLabelValues(http.MethodGet, route)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next(rec, r)

		dur := time.Since(start)
		requestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		requestDuration.WithLabelValues(r.Method, route).Observe(dur.Seconds())
		if rec.status >= 500 {
			errorsTotal.WithLabelValues(r.Method, route).Inc()
		}

		logFrom(r.Context()).Info("request handled",
			"method", r.Method,
			"path", route,
			"status", rec.status,
			"duration_ms", dur.Milliseconds(),
		)
	})
}

// ---------- Handlers ----------

func healthHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "ok")
}

func failHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	err := errors.New("simulated failure")

	// mark the current (root) span as failed -> red in Jaeger
	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())

	logFrom(ctx).Error("fail handler triggered", "error", err.Error())
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func slowHandler(w http.ResponseWriter, r *http.Request) {
	// nested span: shows up in the Jaeger waterfall under the root span
	ctx, span := tracer.Start(r.Context(), "slow-op")
	defer span.End()

	d := time.Duration(1000+rand.IntN(2001)) * time.Millisecond // 1000..3000 ms
	span.SetAttributes(attribute.Int64("slow.sleep_ms", d.Milliseconds()))
	logFrom(ctx).Info("slow operation started", "sleep_ms", d.Milliseconds())

	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
	fmt.Fprintf(w, "slept %s", d)
}

// loadHandler fires a burst of requests at the service itself.
// /load?n=200&path=/health   (path: /health, /fail or /slow)
func loadHandler(selfURL string) http.HandlerFunc {
	allowed := map[string]bool{"/health": true, "/fail": true, "/slow": true}
	client := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport), // propagates trace context
		Timeout:   15 * time.Second,
	}

	return func(w http.ResponseWriter, r *http.Request) {
		n := 200
		if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v > 0 && v <= 5000 {
			n = v
		}
		path := r.URL.Query().Get("path")
		if !allowed[path] {
			path = "/health"
		}

		const concurrency = 50
		sem := make(chan struct{}, concurrency)
		var wg sync.WaitGroup
		var okCount, failCount atomic.Int64

		logFrom(r.Context()).Info("load burst started", "n", n, "target", path)

		for i := 0; i < n; i++ {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, selfURL+path, nil)
				if err != nil {
					failCount.Add(1)
					return
				}
				resp, err := client.Do(req)
				if err != nil {
					failCount.Add(1)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 500 {
					failCount.Add(1)
				} else {
					okCount.Add(1)
				}
			}()
		}
		wg.Wait()

		fmt.Fprintf(w, "sent %d requests to %s: ok=%d failed=%d\n", n, path, okCount.Load(), failCount.Load())
	}
}

// ---------- main ----------

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := initTracing(ctx)
	if err != nil {
		slog.Error("failed to init tracing", "error", err.Error())
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	selfURL := os.Getenv("SELF_URL")
	if selfURL == "" {
		selfURL = "http://127.0.0.1:" + port
	}

	mux := http.NewServeMux()
	mux.Handle("GET /health", instrument("/health", healthHandler))
	mux.Handle("GET /fail", instrument("/fail", failHandler))
	mux.Handle("GET /slow", instrument("/slow", slowHandler))
	mux.Handle("GET /load", instrument("/load", loadHandler(selfURL)))
	mux.Handle("GET /metrics", promhttp.Handler())

	// otelhttp creates the root span for every incoming request
	handler := otelhttp.NewHandler(mux, "lab-api",
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/metrics" }),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("server starting", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err.Error())
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	_ = shutdownTracing(shutdownCtx) // flush pending spans
}
