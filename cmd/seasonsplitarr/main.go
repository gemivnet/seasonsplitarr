package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gemivnet/seasonsplitarr/internal/config"
	"github.com/gemivnet/seasonsplitarr/internal/debrid"
	"github.com/gemivnet/seasonsplitarr/internal/grabber"
	"github.com/gemivnet/seasonsplitarr/internal/logging"
	"github.com/gemivnet/seasonsplitarr/internal/qbittorrent"
	"github.com/gemivnet/seasonsplitarr/internal/store"
	"github.com/gemivnet/seasonsplitarr/internal/torznab"
)

func main() {
	// Make stdlib log emit microseconds + file:line so the trail through the
	// stack is easy to reconstruct from `docker logs`.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	logger := logging.New("main")
	logger.Info("seasonsplitarr starting up (pid=%d)", os.Getpid())

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config: %v", err)
		log.Fatalf("config: %v", err)
	}

	logger.Info("config loaded:")
	logger.Info("  SS_LISTEN              = %s", cfg.Listen)
	logger.Info("  SS_UPSTREAM_URL        = %s", cfg.UpstreamURL)
	logger.Info("  SS_UPSTREAM_APIKEY     = %s", logging.Redact(cfg.UpstreamAPIKey))
	logger.Info("  SS_APIKEY              = %s (len=%d)", logging.Redact(cfg.APIKey), len(cfg.APIKey))
	logger.Info("  SS_QBIT_USERNAME       = %s", cfg.QBitUsername)
	logger.Info("  SS_QBIT_PASSWORD       = %s (len=%d)", logging.Redact(cfg.QBitPassword), len(cfg.QBitPassword))
	logger.Info("  SS_REALDEBRID_TOKEN    = %s", logging.Redact(cfg.RealDebridToken))
	logger.Info("  SS_DOWNLOADS_DIR       = %s", cfg.DownloadsDir)

	// 0o755: shared with Sonarr's bind-mounted import path.
	if err := os.MkdirAll(cfg.DownloadsDir, 0o755); err != nil { // #nosec G301 -- shared-volume scenario
		logger.Error("downloads dir mkdir: %v", err)
		log.Fatalf("downloads dir: %v", err)
	}
	logger.Info("downloads dir ready: %s", cfg.DownloadsDir)

	statePath := filepath.Join(cfg.DownloadsDir, ".state.json")
	st, err := store.Open(statePath)
	if err != nil {
		logger.Error("store open %s: %v", statePath, err)
		log.Fatalf("store: %v", err)
	}
	logger.Info("store opened: %s (existing grabs: %d)", statePath, len(st.List()))

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
		_, _ = w.Write([]byte("ok"))
	})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Grabber polls the store and drives RD downloads.
	logger.Info("starting grabber loop")
	go gr.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           logging.RequestLog("http", mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		logger.Info("shutdown signal received, draining HTTP server")
		_ = srv.Shutdown(context.Background())
	}()

	logger.Info("seasonsplitarr listening on %s", cfg.Listen)
	logger.Info("  torznab:   http://%s/torznab/api", cfg.Listen)
	logger.Info("  qbit shim: http://%s/api/v2/", cfg.Listen)
	logger.Info("  healthz:   http://%s/healthz", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("listen: %v", err)
		log.Fatal(err)
	}
	logger.Info("seasonsplitarr exited cleanly")
}
