// Package store persists seasonsplitarr's per-grab state to a JSON file.
// Sqlite was tempting but would pull in CGO; the working set fits trivially
// in memory and Sonarr's call rate is low.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gemivnet/seasonsplitarr/internal/logging"
)

var slog = logging.New("store")

type State string

const (
	// StateRegistered means a synthetic per-season release has been seen in
	// a search response (via OnSynthetic) but Sonarr has not yet grabbed it.
	// The grabber must NOT touch these — otherwise every search would burn
	// RD addMagnet quota for results the user never asked for. handleAdd
	// promotes a Registered grab to Queued when Sonarr clicks download.
	StateRegistered  State = "registered"
	StateQueued      State = "queued"      // added by Sonarr, not yet sent to RD
	StateDownloading State = "downloading" // RD is fetching, or we are streaming files
	StateReady       State = "ready"       // files materialised under SavePath; Sonarr will import
	StateError       State = "error"
)

// Grab is one Sonarr-side "download" — i.e. one season pulled out of a
// multi-season pack.
type Grab struct {
	// SynthHash is the per-season fake infohash advertised in the synthetic
	// Torznab result. This is what Sonarr knows the torrent by.
	SynthHash string `json:"synth_hash"`
	// GUID is the Torznab GUID for this synthetic release.
	GUID string `json:"guid"`
	// RealHash is the underlying multi-season torrent's infohash.
	RealHash string `json:"real_hash"`
	// Magnet is the original (real) magnet URL.
	Magnet      string    `json:"magnet"`
	Title       string    `json:"title"`
	Season      int       `json:"season"`
	Category    string    `json:"category"`
	State       State     `json:"state"`
	Error       string    `json:"error,omitempty"`
	SavePath    string    `json:"save_path"`     // folder Sonarr scans
	ContentPath string    `json:"content_path"`  // folder containing the files (qBit `content_path`)
	TotalBytes  int64     `json:"total_bytes"`
	DoneBytes   int64     `json:"done_bytes"`
	RDTorrentID string    `json:"rd_torrent_id,omitempty"`
	AddedAt     time.Time `json:"added_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

type Store struct {
	mu    sync.Mutex
	path  string
	grabs map[string]*Grab // keyed by SynthHash
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, grabs: map[string]*Grab{}}
	if path == "" {
		return s, nil
	}
	// 0o755 to allow other *arr containers to read shared parent dirs; the
	// state file itself is written 0o600 in flushLocked.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { // #nosec G301 -- shared-volume scenario
		return nil, err
	}
	// path is operator-controlled (SS_DOWNLOADS_DIR env), not user input.
	b, err := os.ReadFile(path) // #nosec G304 -- operator-controlled path

	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	var dump struct {
		Grabs []*Grab `json:"grabs"`
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &dump); err != nil {
			return nil, fmt.Errorf("decode store: %w", err)
		}
	}
	for _, g := range dump.Grabs {
		s.grabs[g.SynthHash] = g
	}
	return s, nil
}

func (s *Store) Put(g *Grab) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.grabs[g.SynthHash]
	s.grabs[g.SynthHash] = g
	action := "Put new"
	if existed {
		action = "Put overwrite"
	}
	slog.Info("%s: hash=%s title=%q S%02d state=%s", action, g.SynthHash, g.Title, g.Season, g.State)
	return s.flushLocked()
}

func (s *Store) Get(synthHash string) (*Grab, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grabs[synthHash]
	return g, ok
}

func (s *Store) GetByGUID(guid string) (*Grab, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.grabs {
		if g.GUID == guid {
			return g, true
		}
	}
	return nil, false
}

// ByRealHash returns all grabs that share the same underlying torrent.
func (s *Store) ByRealHash(realHash string) []*Grab {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Grab
	for _, g := range s.grabs {
		if g.RealHash == realHash {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Season < out[j].Season })
	return out
}

func (s *Store) List() []*Grab {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Grab, 0, len(s.grabs))
	for _, g := range s.grabs {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AddedAt.Before(out[j].AddedAt) })
	return out
}

func (s *Store) Delete(synthHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.grabs, synthHash)
	slog.Info("Delete: hash=%s", synthHash)
	return s.flushLocked()
}

// Update applies fn to the named grab and persists. Returns false if the
// grab doesn't exist.
func (s *Store) Update(synthHash string, fn func(*Grab)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grabs[synthHash]
	if !ok {
		return false, nil
	}
	prev := g.State
	fn(g)
	if g.State != prev {
		slog.Info("Update: hash=%s state %s -> %s", synthHash, prev, g.State)
	} else {
		slog.Debug("Update: hash=%s (state unchanged: %s, done=%d/%d)", synthHash, g.State, g.DoneBytes, g.TotalBytes)
	}
	return true, s.flushLocked()
}

func (s *Store) flushLocked() error {
	if s.path == "" {
		return nil
	}
	dump := struct {
		Grabs []*Grab `json:"grabs"`
	}{Grabs: make([]*Grab, 0, len(s.grabs))}
	for _, g := range s.grabs {
		dump.Grabs = append(dump.Grabs, g)
	}
	b, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	// 0o600: state contains magnet URLs and RD torrent IDs. Not catastrophic
	// if leaked but no reason for other local users to read it.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
