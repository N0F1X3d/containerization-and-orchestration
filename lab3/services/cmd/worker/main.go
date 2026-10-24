// worker — забирает необработанные заказы из postgres и помечает их обработанными.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"shop/internal/store"
)

var version = "dev"

var (
	log = slog.New(slog.NewJSONHandler(os.Stdout, nil))

	processed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "shop_orders_processed_total", Help: "Сколько заказов обработал worker.",
	})
	errorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "shop_worker_errors_total", Help: "Ошибки при обработке заказов.",
	})
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// HTTP только для проб и метрик — поднимается сразу, ещё до базы.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.Handle("GET /metrics", promhttp.Handler())
	go http.ListenAndServe(":8080", mux)

	db, err := store.Connect(ctx, log)
	if err != nil {
		log.Info("worker stopped before db was ready")
		return
	}
	defer db.Close()

	log.Info("worker started", "version", version)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("worker stopped")
			return
		case <-tick.C:
			n, err := db.ProcessBatch(ctx, 10)
			if err != nil {
				errorsTotal.Inc()
				log.Error("process batch failed", "error", err.Error())
				continue
			}
			if n > 0 {
				processed.Add(float64(n))
				log.Info("orders processed", "count", n)
			}
		}
	}
}
