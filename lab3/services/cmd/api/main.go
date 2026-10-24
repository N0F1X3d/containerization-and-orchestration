// api — HTTP-сервис магазина: принимает заказы и отдаёт их список.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"shop/internal/store"
)

// version задаётся при сборке: -ldflags "-X main.version=1.1".
var version = "dev"

var (
	log = slog.New(slog.NewJSONHandler(os.Stdout, nil))

	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total", Help: "Число HTTP-запросов.",
	}, []string{"path", "code"})
	ordersCreated = promauto.NewCounter(prometheus.CounterOpts{
		Name: "shop_orders_created_total", Help: "Сколько заказов создано.",
	})
)

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) { w.code = code; w.ResponseWriter.WriteHeader(code) }

func observe(path string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		h(sw, r)
		requests.WithLabelValues(path, strconv.Itoa(sw.code)).Inc()
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// HEALTH_FAIL=true — «сломанная версия»: /health отвечает 503, поды не проходят readiness.
	healthFail := os.Getenv("HEALTH_FAIL") == "true"

	// База подключается в фоне: HTTP (и /health) работает сразу, даже пока postgres ещё нет.
	var dbp atomic.Pointer[store.Store]
	go func() {
		if s, err := store.Connect(ctx, log); err == nil {
			dbp.Store(s)
		}
	}()
	// db возвращает подключение или отвечает 503, если базы ещё нет.
	db := func(w http.ResponseWriter) *store.Store {
		s := dbp.Load()
		if s == nil {
			http.Error(w, "database is not ready", http.StatusServiceUnavailable)
		}
		return s
	}

	// Длина очереди считается запросом в базу в момент скрейпа.
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "shop_orders_pending", Help: "Сколько заказов ждут обработки worker'ом.",
	}, func() float64 {
		s := dbp.Load()
		if s == nil {
			return 0
		}
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		n, err := s.Pending(c)
		if err != nil {
			return 0
		}
		return float64(n)
	})

	mux := http.NewServeMux()
	mux.Handle("GET /health", observe("/health", func(w http.ResponseWriter, _ *http.Request) {
		if healthFail {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	}))
	mux.Handle("GET /version", observe("/version", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, version)
	}))
	mux.Handle("POST /order", observe("/order", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Item string `json:"item"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Item == "" {
			req.Item = r.URL.Query().Get("item")
		}
		if req.Item == "" {
			http.Error(w, `usage: POST /order {"item":"..."} or ?item=...`, http.StatusBadRequest)
			return
		}
		s := db(w)
		if s == nil {
			return
		}
		o, err := s.Create(r.Context(), req.Item)
		if err != nil {
			log.Error("create order failed", "error", err.Error())
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		ordersCreated.Inc()
		log.Info("order created", "id", o.ID, "item", o.Item)
		writeJSON(w, http.StatusCreated, o)
	}))
	mux.Handle("GET /orders", observe("/orders", func(w http.ResponseWriter, r *http.Request) {
		s := db(w)
		if s == nil {
			return
		}
		orders, err := s.List(r.Context(), 50)
		if err != nil {
			log.Error("list orders failed", "error", err.Error())
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, orders)
	}))
	mux.Handle("GET /metrics", promhttp.Handler())

	srv := &http.Server{Addr: ":8080", Handler: mux}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(c) // дослужить начатые запросы — важно для rolling update без потерь
	}()

	log.Info("api started", "version", version, "health_fail", healthFail)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "error", err.Error())
		os.Exit(1)
	}
}
