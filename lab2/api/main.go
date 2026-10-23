// api — тестовый сервис для лабы 2: метрики Prometheus (RED), JSON-логи с trace_id, трейсы OpenTelemetry.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
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
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Число обработанных HTTP-запросов.",
	}, []string{"path", "code"})

	failures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_request_errors_total",
		Help: "Число запросов, завершившихся ошибкой 5xx.",
	}, []string{"path"})

	duration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Время ответа на HTTP-запрос.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5},
	}, []string{"path"})

	tracer = otel.Tracer("api")
	log    = slog.New(slog.NewJSONHandler(os.Stdout, nil))
)

// logCtx добавляет к записи лога trace_id и span_id текущего спана.
func logCtx(ctx context.Context) *slog.Logger {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return log
	}
	return log.With("trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// observe — RED-метрики и лог одной строкой на каждый запрос.
func observe(path string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		h(sw, r)
		elapsed := time.Since(start)

		code := strconv.Itoa(sw.code)
		requests.WithLabelValues(path, code).Inc()
		duration.WithLabelValues(path).Observe(elapsed.Seconds())
		level := slog.LevelInfo
		if sw.code >= 500 {
			failures.WithLabelValues(path).Inc()
			level = slog.LevelError
		}
		logCtx(r.Context()).Log(r.Context(), level, "request",
			"method", r.Method, "path", path, "status", sw.code, "duration_ms", elapsed.Milliseconds())
	})
}

func health(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintln(w, "ok")
}

// fail — ошибка 5xx, спан помечается как ошибочный.
func fail(w http.ResponseWriter, r *http.Request) {
	err := errors.New("simulated failure: database unavailable")
	span := trace.SpanFromContext(r.Context())
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	logCtx(r.Context()).Error("request failed", "error", err.Error())
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// slow — медленная операция во вложенном спане slow-op.
func slow(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "slow-op")
	d := time.Duration(1000+rand.IntN(2000)) * time.Millisecond
	span.SetAttributes(attribute.Int64("sleep_ms", d.Milliseconds()))
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
	span.End()
	fmt.Fprintf(w, "slow: %s\n", d)
}

// load — пачка запросов к самому себе, чтобы подскочил RPS. Клиент с otelhttp,
// поэтому дочерние запросы попадают в тот же трейс.
func load(self string) http.HandlerFunc {
	client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport), Timeout: 10 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		n := 50
		if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v > 0 && v <= 1000 {
			n = v
		}
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, self+"/health", nil)
				if resp, err := client.Do(req); err == nil {
					resp.Body.Close()
				}
			}()
		}
		wg.Wait()
		fmt.Fprintf(w, "sent %d requests\n", n)
	}
}

func setupTracing(ctx context.Context) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	// Адрес не задан — трейсы никуда не шлём (иначе экспортёр долбится в localhost:4318 и сыплет ошибками).
	// Спаны всё равно создаются, поэтому trace_id в логах есть всегда.
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		otel.SetTracerProvider(sdktrace.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}
	// Адрес берётся из OTEL_EXPORTER_OTLP_ENDPOINT, имя сервиса — из OTEL_SERVICE_NAME.
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdown, err := setupTracing(ctx)
	if err != nil {
		log.Error("tracing setup failed", "error", err.Error())
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.Handle("/health", observe("/health", health))
	mux.Handle("/fail", observe("/fail", fail))
	mux.Handle("/slow", observe("/slow", slow))
	mux.Handle("/load", observe("/load", load("http://127.0.0.1:"+port)))
	mux.Handle("/metrics", promhttp.Handler())

	// otelhttp создаёт корневой спан на каждый входящий запрос (кроме /metrics — это шум).
	handler := otelhttp.NewHandler(mux, "http",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + r.URL.Path }),
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/metrics" }),
	)

	srv := &http.Server{Addr: ":" + port, Handler: handler}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		shutdown(shutdownCtx)
	}()

	log.Info("api started", "port", port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "error", err.Error())
		os.Exit(1)
	}
}
