// Package qbittorrent implements a minimal subset of the qBittorrent
// WebUI API surface that Sonarr (and Radarr) interact with. It is not a real
// torrent client — it accepts grabs from Sonarr, delegates the actual fetch
// to Real-Debrid (via the grabber), and reports back synthetic state
// pointing at per-season folders.
//
// Endpoints implemented (Sonarr-relevant subset):
//
//	POST /api/v2/auth/login              -> "Ok."
//	GET  /api/v2/app/version             -> "v4.6.0" (lies; Sonarr just checks present)
//	GET  /api/v2/app/webapiVersion       -> "2.9.3"
//	GET  /api/v2/app/preferences         -> minimal JSON
//	POST /api/v2/torrents/add            -> "Ok." (registers a grab)
//	GET  /api/v2/torrents/info           -> list of qBit-shaped torrents
//	GET  /api/v2/torrents/properties     -> {}
//	GET  /api/v2/torrents/files          -> []
//	POST /api/v2/torrents/delete         -> "Ok."
//	POST /api/v2/torrents/setCategory    -> "Ok."
//	POST /api/v2/torrents/createCategory -> "Ok."
//	GET  /api/v2/torrents/categories     -> {}
package qbittorrent

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gemivnet/seasonsplitarr/internal/store"
	"github.com/gemivnet/seasonsplitarr/internal/torznab"
)

// isInfohash returns true if s is a 40-char lowercase hex string. We require
// this before any value derived from a user-supplied magnet is used in a
// filesystem path, to prevent traversal.
func isInfohash(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

type Shim struct {
	// DownloadsDir is the root under which per-grab/per-season folders are
	// materialised. Sonarr import-scans inside SavePath = DownloadsDir/<grab>.
	DownloadsDir string
	Store        *store.Store

	session *session
}

// NewShim constructs a Shim with credential-protected qBit endpoints. The
// caller must provide non-empty username and password; auth bypass on the
// download client surface would expose the user's Real-Debrid quota and
// allow arbitrary library writes.
func NewShim(downloadsDir, username, password string, st *store.Store) *Shim {
	return &Shim{
		DownloadsDir: downloadsDir,
		Store:        st,
		session:      newSession(username, password),
	}
}

func (s *Shim) Handler() http.Handler {
	mux := http.NewServeMux()

	// Login is intentionally unauthenticated — that's how Sonarr obtains the
	// SID cookie. Everything else requires the cookie via requireAuth.
	mux.HandleFunc("/api/v2/auth/login", s.handleLogin)
	mux.HandleFunc("/api/v2/auth/logout", s.handleLogout)

	mux.HandleFunc("/api/v2/app/version", s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "v4.6.0")
	}))
	mux.HandleFunc("/api/v2/app/webapiVersion", s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "2.9.3")
	}))
	mux.HandleFunc("/api/v2/app/preferences", s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"save_path":         s.DownloadsDir,
			"temp_path":         s.DownloadsDir,
			"temp_path_enabled": false,
			"queueing_enabled":  false,
			"dht":               true,
		})
	}))

	mux.HandleFunc("/api/v2/torrents/add", s.requireAuth(s.handleAdd))
	mux.HandleFunc("/api/v2/torrents/info", s.requireAuth(s.handleInfo))
	mux.HandleFunc("/api/v2/torrents/properties", s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{})
	}))
	mux.HandleFunc("/api/v2/torrents/files", s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []any{})
	}))
	mux.HandleFunc("/api/v2/torrents/delete", s.requireAuth(s.handleDelete))
	mux.HandleFunc("/api/v2/torrents/setCategory", s.requireAuth(okHandler))
	mux.HandleFunc("/api/v2/torrents/createCategory", s.requireAuth(okHandler))
	mux.HandleFunc("/api/v2/torrents/categories", s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{})
	}))

	return mux
}

func (s *Shim) handleLogin(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	user := r.FormValue("username")
	pass := r.FormValue("password")
	if !s.session.validate(user, pass) {
		// Match qBittorrent's response shape: 200 with "Fails." body.
		// Sonarr keys off the body, not the status, so don't change either.
		fmt.Fprint(w, "Fails.")
		return
	}
	tok := s.session.issue()
	// Secure=false intentionally: Sonarr typically reaches seasonsplitarr
	// over plain HTTP inside the docker network. With Secure=true the client
	// would refuse to send the cookie back, breaking auth. If you expose
	// this to the internet, run it behind a TLS-terminating reverse proxy.
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- see comment above
		Name:     "SID",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   24 * 60 * 60,
	})
	fmt.Fprint(w, "Ok.")
}

func (s *Shim) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("SID"); err == nil {
		s.session.revoke(c.Value)
	}
	// Same Secure=false rationale as in handleLogin.
	http.SetCookie(w, &http.Cookie{Name: "SID", Value: "", Path: "/", MaxAge: -1}) // #nosec G124
	fmt.Fprint(w, "Ok.")
}

// handleAdd accepts Sonarr's grab. Sonarr submits a multipart form whose
// `urls` field contains the magnet (newline-separated if multiple). We pull
// out the first magnet, extract its synthetic infohash, look up the grab
// metadata that was recorded when we minted the synthetic Torznab result,
// and register a store.Grab so the grabber can take it from here.
func (s *Shim) handleAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		_ = r.ParseForm() // tolerate clients that send urlencoded
	}
	urls := strings.TrimSpace(r.FormValue("urls"))
	category := r.FormValue("category")
	if urls == "" {
		http.Error(w, "missing urls", http.StatusBadRequest)
		return
	}
	magnet := strings.TrimSpace(strings.Split(urls, "\n")[0])
	synthHash := infohashFromMagnet(magnet)
	if !isInfohash(synthHash) {
		http.Error(w, "invalid magnet infohash", http.StatusBadRequest)
		return
	}

	// Two paths: (a) we already have a synthetic-feed entry registered for
	// this hash, just attach the magnet/category to it; (b) Sonarr is adding
	// a torrent that we have no record of (manual paste, non-split release).
	// For (b) we register a passthrough grab — single-season, no real-hash
	// mapping. The grabber treats it the same way as a synthetic grab; the
	// season detector still works on the file list.
	if _, ok := s.Store.Get(synthHash); ok {
		_, _ = s.Store.Update(synthHash, func(gr *store.Grab) {
			gr.Magnet = magnet
			gr.Category = category
		})
	} else {
		g := &store.Grab{
			SynthHash: synthHash,
			RealHash:  synthHash, // unknown real hash; treat as itself
			Magnet:    magnet,
			Title:     displayNameFromMagnet(magnet),
			Category:  category,
			State:     store.StateQueued,
			SavePath:  filepath.Join(s.DownloadsDir, synthHash),
			AddedAt:   time.Now().UTC(),
		}
		_ = s.Store.Put(g)
	}
	fmt.Fprint(w, "Ok.")
}

// handleInfo returns a list of torrents in qBit shape. Sonarr polls this and
// matches by hash to track its grabs.
func (s *Shim) handleInfo(w http.ResponseWriter, r *http.Request) {
	wantHashes := splitCSV(r.URL.Query().Get("hashes"))
	wantCategory := r.URL.Query().Get("category")

	out := make([]map[string]any, 0)
	for _, g := range s.Store.List() {
		if len(wantHashes) > 0 && !contains(wantHashes, g.SynthHash) {
			continue
		}
		if wantCategory != "" && g.Category != wantCategory {
			continue
		}
		out = append(out, torrentInfoJSON(g))
	}
	writeJSON(w, out)
}

func (s *Shim) handleDelete(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	for _, h := range splitCSV(r.FormValue("hashes")) {
		h = strings.ToLower(strings.TrimSpace(h))
		if !isInfohash(h) {
			continue
		}
		_ = s.Store.Delete(h)
	}
	fmt.Fprint(w, "Ok.")
}

// torrentInfoJSON renders a store.Grab as a qBit-shaped torrent record.
// Field set chosen to match what Sonarr's qBit client reads. Fields it
// doesn't care about are still present with plausible defaults so qBit
// schema parsers (which can be picky) don't choke.
func torrentInfoJSON(g *store.Grab) map[string]any {
	state := "downloading"
	progress := 0.0
	if g.TotalBytes > 0 {
		progress = float64(g.DoneBytes) / float64(g.TotalBytes)
	}
	switch g.State {
	case store.StateReady:
		state = "pausedUP" // "completed, seeding paused" — Sonarr triggers import.
		progress = 1.0
	case store.StateError:
		state = "error"
	case store.StateQueued:
		state = "queuedDL"
	}

	name := g.Title
	if name == "" {
		name = filepath.Base(g.SavePath)
	}
	contentPath := g.ContentPath
	if contentPath == "" {
		contentPath = g.SavePath
	}

	return map[string]any{
		"hash":          g.SynthHash,
		"name":          name,
		"size":          g.TotalBytes,
		"progress":      progress,
		"dlspeed":       0,
		"upspeed":       0,
		"priority":      1,
		"num_seeds":     0,
		"num_complete":  0,
		"num_leechs":    0,
		"num_incomplete": 0,
		"ratio":         0.0,
		"eta":           0,
		"state":         state,
		"seq_dl":        false,
		"f_l_piece_prio": false,
		"category":      g.Category,
		"tags":          "",
		"super_seeding": false,
		"force_start":   false,
		"save_path":     g.SavePath,
		"content_path":  contentPath,
		"added_on":      g.AddedAt.Unix(),
		"completion_on": g.CompletedAt.Unix(),
		"tracker":       "",
		"dl_limit":      -1,
		"up_limit":      -1,
		"downloaded":    g.DoneBytes,
		"uploaded":      0,
		"downloaded_session": g.DoneBytes,
		"uploaded_session":   0,
		"amount_left":   g.TotalBytes - g.DoneBytes,
		"completed":     g.DoneBytes,
		"max_ratio":     -1,
		"max_seeding_time": -1,
		"ratio_limit":   -2,
		"seeding_time_limit": -2,
		"seen_complete":  0,
		"last_activity":  time.Now().Unix(),
		"time_active":    int64(time.Since(g.AddedAt).Seconds()),
		"auto_tmm":       false,
		"total_size":     g.TotalBytes,
	}
}

func displayNameFromMagnet(magnet string) string {
	const dn = "dn="
	i := strings.Index(magnet, dn)
	if i < 0 {
		return ""
	}
	rest := magnet[i+len(dn):]
	if amp := strings.IndexAny(rest, "&"); amp >= 0 {
		rest = rest[:amp]
	}
	return rest
}

// RegisterSynthetic is called by the Torznab proxy when it mints a synthetic
// per-season release. Capturing this lets handleAdd resolve the real
// underlying magnet/season later without parsing the synthetic title.
//
// In the v0.1 design the proxy doesn't yet wire this in (search responses
// stream straight back to Sonarr without state), so handleAdd falls back to
// passthrough-grab behavior. This hook is the place to plug in caching once
// we want correctness for re-search-then-grab flows.
func (s *Shim) RegisterSynthetic(synthHash, realHash, magnet string, season int, title string) {
	if !isInfohash(synthHash) || !isInfohash(realHash) {
		return
	}
	g := &store.Grab{
		SynthHash: synthHash,
		RealHash:  realHash,
		Magnet:    magnet,
		Title:     title,
		Season:    season,
		State:     store.StateQueued,
		SavePath:  filepath.Join(s.DownloadsDir, synthHash),
		AddedAt:   time.Now().UTC(),
	}
	_ = s.Store.Put(g)
}

// --- helpers ---

func okHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "Ok.")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "|")
	if len(parts) == 1 {
		parts = strings.Split(s, ",")
	}
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

func infohashFromMagnet(s string) string {
	const prefix = "magnet:?"
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	const xt = "xt=urn:btih:"
	j := strings.Index(s[i:], xt)
	if j < 0 {
		return ""
	}
	rest := s[i+j+len(xt):]
	if amp := strings.IndexAny(rest, "&"); amp >= 0 {
		rest = rest[:amp]
	}
	return strings.ToLower(rest)
}

// Compile-time check torznab import is used so future refactors notice.
var _ = torznab.SyntheticInfohash
