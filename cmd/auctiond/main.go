// Command auctiond 启动密封竞价拍卖服务的 HTTP 进程。
//
//	AUCTION_DATA_DIR 指向目录时使用 JSON 文件持久化（默认）；
//	AUCTION_DATA_DIR=memory 时使用纯内存存储。
//	ADDR 为监听地址，默认 :8080。
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	auction "github.com/chris64233/go-auction-clearing"
)

func main() {
	dir := os.Getenv("AUCTION_DATA_DIR")
	var repo auction.Repository
	if dir == "" {
		dir = "./data"
	}
	if dir == "memory" {
		repo = auction.NewMemoryRepository()
		log.Print("storage: in-memory")
	} else {
		r, err := auction.LoadFileRepository(dir)
		if err != nil {
			log.Fatalf("load repository %s: %v", dir, err)
		}
		repo = r
		log.Printf("storage: file directory %s", dir)
	}

	svc := auction.NewService(repo, auction.SystemClock{})
	server := &http.Server{
		Addr:              addr(),
		Handler:           auction.NewServer(svc),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("auction service listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

func addr() string {
	if a := os.Getenv("ADDR"); a != "" {
		return a
	}
	return ":8080"
}
