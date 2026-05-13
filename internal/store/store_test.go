package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := &Grab{
		SynthHash: "aaaa",
		GUID:      "guid-1",
		RealHash:  "bbbb",
		Title:     "Show.S03.x264",
		Season:    3,
		State:     StateQueued,
		AddedAt:   time.Now().UTC().Truncate(time.Second),
	}
	if err := s.Put(g); err != nil {
		t.Fatal(err)
	}

	// Reopen and verify persistence.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get("aaaa")
	if !ok {
		t.Fatal("grab not found after reopen")
	}
	if got.Title != g.Title || got.Season != g.Season {
		t.Errorf("got %+v, want %+v", got, g)
	}

	// Update and persist.
	if _, err := s2.Update("aaaa", func(g *Grab) { g.State = StateReady }); err != nil {
		t.Fatal(err)
	}
	s3, _ := Open(path)
	got2, _ := s3.Get("aaaa")
	if got2.State != StateReady {
		t.Errorf("state = %q, want %q", got2.State, StateReady)
	}
}

func TestByRealHash(t *testing.T) {
	s, _ := Open("")
	for i, h := range []string{"a", "b", "c"} {
		s.Put(&Grab{SynthHash: h, RealHash: "shared", Season: i + 1})
	}
	s.Put(&Grab{SynthHash: "d", RealHash: "other", Season: 1})
	grabs := s.ByRealHash("shared")
	if len(grabs) != 3 {
		t.Errorf("got %d, want 3", len(grabs))
	}
	for i, g := range grabs {
		if g.Season != i+1 {
			t.Errorf("seasons not sorted: %v", grabs)
			break
		}
	}
}
