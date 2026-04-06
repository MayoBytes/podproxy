package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"podproxy/internal/api"
	"podproxy/internal/config"
	"podproxy/internal/db"
	"podproxy/internal/feed"
	"podproxy/internal/proxy"
	"podproxy/internal/ui"
)

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	database, err := db.Open(cfg.Storage.DataDir)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()

	if err := database.ReconcileOnStartup(); err != nil {
		log.Fatalf("reconcile cache state: %v", err)
	}

	fetcher := feed.NewFetcher(cfg)

	prefetcher := feed.NewPrefetcher(database, cfg)
	prefetcher.Start()

	poller := feed.NewPoller(database, fetcher, prefetcher)
	poller.Start()

	mux := http.NewServeMux()
	api.RegisterRoutes(mux, database, fetcher, prefetcher, cfg)
	proxy.RegisterRoutes(mux, database, fetcher, prefetcher, cfg)
	ui.RegisterRoutes(mux, database, fetcher, prefetcher, cfg)

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		stats, err := database.GetGlobalStats()
		if err != nil {
			log.Printf("health: get stats: %v", err)
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"status":"error"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":          "ok",
			"feeds":           stats.FeedCount,
			"episodes":        stats.EpisodeCount,
			"cached_episodes": stats.CachedCount,
			"disk_bytes":      stats.DiskBytes,
		})
	})

	srv := &http.Server{
		Addr:         cfg.Addr(),
		Handler:      mux,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 0, // streaming responses; let the proxy handle timeouts
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("podproxy listening on %s (base_url: %s)", srv.Addr, cfg.Server.BaseURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down...")
	// Drain active HTTP connections before stopping background workers.
	// Handlers may call prefetcher.Enqueue; stopping the prefetcher first
	// (which closes its queue channel) would cause a panic on those sends.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	poller.Stop()
	prefetcher.Stop()
	log.Println("stopped")
}
