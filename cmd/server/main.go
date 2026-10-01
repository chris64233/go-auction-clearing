// Command auction-server 启动多单位密封竞价清算 HTTP 服务。
//
// 用法：
//
//	auction-server -addr :8080 -events ./data/events.jsonl
//
// 事件日志不存在会自动创建；重启后自动重放恢复全部拍卖与成交结果。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	auctionclearing "github.com/chris64233/go-auction-clearing"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	eventsPath := flag.String("events", "data/events.jsonl", "path to the event log (JSON Lines)")
	flag.Parse()

	// 自动创建事件日志所在目录（日志文件本身由 FileEventStore 创建）。
	if dir := filepath.Dir(*eventsPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("create event log directory: %v", err)
		}
	}

	store, err := auctionclearing.NewFileEventStore(*eventsPath)
	if err != nil {
		log.Fatalf("open event store: %v", err)
	}
	defer store.Close()

	engine, err := auctionclearing.NewEngine(store, auctionclearing.SystemClock{})
	if err != nil {
		log.Fatalf("init engine: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           auctionclearing.NewServer(engine).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("auction server listening on %s (event log: %s)", *addr, *eventsPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown: %v", err)
		os.Exit(1)
	}
	log.Println("stopped")
}
