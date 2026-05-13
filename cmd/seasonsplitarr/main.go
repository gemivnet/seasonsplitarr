package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/gemivnet/seasonsplitarr/internal/config"
	"github.com/gemivnet/seasonsplitarr/internal/debrid"
	"github.com/gemivnet/seasonsplitarr/internal/grabber"
	"github.com/gemivnet/seasonsplitarr/internal/qbittorrent"
	"github.com/gemivnet/seasonsplitarr/internal/store"
	"github.com/gemivnet/seasonsplitarr/internal/torznab"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if err := os.MkdirAll(cfg.DownloadsDir, 0o755); err != nil {
		log.Fatalf("downloads dir: %v", err)
	}

	st, err := store.Open(filepath.Join(cfg.DownloadsDir, ".state.json"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	rd := debrid.New(cfg.RealDebridToken)
	gr := &grabber.Grabber{
		Store:        st,
		RD:           rd,
		DownloadsDir: cfg.DownloadsDir,
	}

	shim := qbittorrent.NewShim(cfg.DownloadsDir, cfg.QBitUsername, cfg.QBitPassword, st)

	proxy := &torznab.Proxy{
		UpstreamURL:    cfg.UpstreamURL,
		UpstreamAPIKey: cfg.UpstreamAPIKey,
		LocalAPIKey:    cfg.APIKey,
		OnSynthetic:    shim.RegisterSynthetic,
	}

	mux := http.NewServeMux()
	mux.Handle("/torznab/", http.StripPrefix("/torznab", proxy.Handler()))
	mux.Handle("/api/v2/", shim.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Grabber polls the store and drives RD downloads.
	go gr.Run(ctx)

	srv := &http.Server{Addr: cfg.Listen, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	log.Printf("seasonsplitarr listening on %s", cfg.Listen)
	log.Printf("  torznab:   http://%s/torznab/api", cfg.Listen)
	log.Printf("  qbit shim: http://%s/api/v2/", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
