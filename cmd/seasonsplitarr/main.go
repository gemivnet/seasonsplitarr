package main

import (
	"log"
	"net/http"

	"github.com/gemivnet/seasonsplitarr/internal/config"
	"github.com/gemivnet/seasonsplitarr/internal/qbittorrent"
	"github.com/gemivnet/seasonsplitarr/internal/torznab"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	proxy := &torznab.Proxy{
		UpstreamURL:    cfg.UpstreamURL,
		UpstreamAPIKey: cfg.UpstreamAPIKey,
		LocalAPIKey:    cfg.APIKey,
	}
	shim := &qbittorrent.Shim{DownloadsDir: cfg.DownloadsDir}

	mux := http.NewServeMux()
	mux.Handle("/torznab/", http.StripPrefix("/torznab", proxy.Handler()))
	mux.Handle("/api/v2/", shim.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	log.Printf("seasonsplitarr listening on %s", cfg.Listen)
	log.Printf("  torznab:   http://%s/torznab/api", cfg.Listen)
	log.Printf("  qbit shim: http://%s/api/v2/", cfg.Listen)
	if err := http.ListenAndServe(cfg.Listen, mux); err != nil {
		log.Fatal(err)
	}
}
