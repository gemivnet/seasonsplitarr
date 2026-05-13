// Package qbittorrent implements a minimal subset of the qBittorrent
// WebUI API surface that Sonarr (and Radarr) interact with. It is not a real
// torrent client — it accepts grabs from Sonarr, delegates the actual fetch
// to Real-Debrid, and reports back synthetic "completed" state pointing at
// per-season folders.
//
// Endpoints implemented (Sonarr-relevant subset):
//   POST /api/v2/auth/login            -> "Ok."
//   GET  /api/v2/app/version           -> "v4.6.0" (lie, but compatible)
//   GET  /api/v2/app/webapiVersion     -> "2.9.3"
//   GET  /api/v2/app/preferences       -> minimal JSON
//   POST /api/v2/torrents/add          -> "Ok."
//   GET  /api/v2/torrents/info         -> [] (or status from store)
//   GET  /api/v2/torrents/properties   -> {}
//   GET  /api/v2/torrents/files        -> []
//   POST /api/v2/torrents/delete       -> "Ok."
//   POST /api/v2/torrents/setCategory  -> "Ok."
//   POST /api/v2/torrents/createCategory -> "Ok."
//   GET  /api/v2/torrents/categories   -> {}
package qbittorrent

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type Shim struct {
	// DownloadsDir is the root under which per-grab/per-season folders are
	// materialised. Sonarr import-scans below here.
	DownloadsDir string
}

func (s *Shim) Handler() http.Handler {
	mux := http.NewServeMux()

	// Auth — Sonarr POSTs username/password; we accept anything.
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Ok.")
	})

	// App info
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "v4.6.0")
	})
	mux.HandleFunc("/api/v2/app/webapiVersion", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "2.9.3")
	})
	mux.HandleFunc("/api/v2/app/preferences", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"save_path":          s.DownloadsDir,
			"temp_path":          s.DownloadsDir,
			"temp_path_enabled":  false,
			"queueing_enabled":   false,
			"dht":                true,
		})
	})

	// Torrents
	mux.HandleFunc("/api/v2/torrents/add", s.handleAdd)
	mux.HandleFunc("/api/v2/torrents/info", s.handleInfo)
	mux.HandleFunc("/api/v2/torrents/properties", s.handleProperties)
	mux.HandleFunc("/api/v2/torrents/files", s.handleFiles)
	mux.HandleFunc("/api/v2/torrents/delete", okHandler)
	mux.HandleFunc("/api/v2/torrents/setCategory", okHandler)
	mux.HandleFunc("/api/v2/torrents/createCategory", okHandler)
	mux.HandleFunc("/api/v2/torrents/categories", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{})
	})

	return mux
}

func (s *Shim) handleAdd(w http.ResponseWriter, r *http.Request) {
	// TODO: parse multipart form, extract `urls` (magnet) and `category`,
	// register the grab in the store, dispatch to RD adder.
	fmt.Fprint(w, "Ok.")
}

func (s *Shim) handleInfo(w http.ResponseWriter, r *http.Request) {
	// TODO: enumerate active grabs from store, return qBit-shaped objects.
	writeJSON(w, []any{})
}

func (s *Shim) handleProperties(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{})
}

func (s *Shim) handleFiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, []any{})
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "Ok.")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
