// batch — заглушка «ночной аналитики»: в бесконечном цикле жрёт CPU и память, ничего полезного не делая.
//
//	BATCH_MEMORY_MB — сколько мегабайт держать (по умолчанию 64);
//	BATCH_CPU_WORKERS — сколько горутин крутят CPU (по умолчанию 1).
package main

import (
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v >= 0 {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	memMB := envInt("BATCH_MEMORY_MB", 64)
	workers := envInt("BATCH_CPU_WORKERS", 1)

	// Память: выделяем и пишем в каждую страницу, иначе ядро её реально не выдаст.
	// Держим постепенно, по 16 МБ в секунду, — чтобы давление на ноду росло плавно.
	var hold [][]byte
	go func() {
		for len(hold)*16 < memMB {
			chunk := make([]byte, 16<<20)
			for i := 0; i < len(chunk); i += 4096 {
				chunk[i] = 1
			}
			hold = append(hold, chunk)
			time.Sleep(time.Second)
		}
		log.Info("memory allocated", "mb", len(hold)*16)
	}()

	// CPU: бесконечные циклы.
	for i := 0; i < workers; i++ {
		go func() {
			x := 0
			for {
				x++
			}
		}()
	}

	log.Info("batch started", "memory_mb", memMB, "cpu_workers", workers)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("batch stopped")
}
