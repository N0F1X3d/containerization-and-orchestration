// api — тестовый сервис для лабы 1: им проверяются namespaces, cgroups и права.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
)

var (
	mu    sync.Mutex
	eaten [][]byte // держим ссылки, чтобы GC не вернул память
)

func health(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintln(w, "ok")
}

// eat выделяет N МБ и держит их до конца жизни процесса.
func eat(w http.ResponseWriter, r *http.Request) {
	mb, err := strconv.Atoi(r.URL.Query().Get("mb"))
	if err != nil || mb <= 0 {
		http.Error(w, "usage: /eat?mb=N (N > 0)", http.StatusBadRequest)
		return
	}

	buf := make([]byte, mb<<20)
	// Пишем в каждую страницу: без этого ядро не выделит физическую память,
	// и RSS (а значит, и счётчик memory cgroup) не вырастет.
	for i := 0; i < len(buf); i += os.Getpagesize() {
		buf[i] = 1
	}

	mu.Lock()
	eaten = append(eaten, buf)
	total := 0
	for _, b := range eaten {
		total += len(b) >> 20
	}
	mu.Unlock()

	log.Printf("eat: +%d MB, holding %d MB", mb, total)
	fmt.Fprintf(w, "holding %d MB\n", total)
}

// burn запускает бесконечный цикл в отдельной горутине — это одно ядро на 100%.
// Каждый вызов добавляет ещё один цикл.
func burn(w http.ResponseWriter, _ *http.Request) {
	go func() {
		for {
		}
	}()
	log.Print("burn: started a busy loop")
	fmt.Fprintln(w, "burning one core")
}

func main() {
	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}

	http.HandleFunc("/health", health)
	http.HandleFunc("/eat", eat)
	http.HandleFunc("/burn", burn)

	host, _ := os.Hostname()
	log.Printf("api: pid=%d uid=%d host=%s listening on %s", os.Getpid(), os.Getuid(), host, addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
