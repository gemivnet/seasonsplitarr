// Package grabber drives the lifecycle of a Sonarr grab end-to-end:
//
//   queued       Sonarr added a synthetic season release.
//      ↓
//   downloading  The real torrent is registered with Real-Debrid, files for
//                this grab's season are selected, and the season's files
//                are being fetched to disk (cached and hardlinked into the
//                grab's SavePath).
//      ↓
//   ready        Files are present under ContentPath; Sonarr's import scan
//                will see only this season's episodes.
//
// The grabber is intentionally simple: a ticker, a worklist scanned from the
// store, and a per-grab state machine. There's no queue worker pool; Real-
// Debrid is the throttle.
package grabber

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gemivnet/seasonsplitarr/internal/debrid"
	"github.com/gemivnet/seasonsplitarr/internal/seasonparse"
	"github.com/gemivnet/seasonsplitarr/internal/store"
)

type Grabber struct {
	Store        *store.Store
	RD           *debrid.Client
	DownloadsDir string
	// PollInterval controls how often the grabber scans the store for work.
	PollInterval time.Duration
	// inflight prevents two concurrent ticks from operating on the same grab.
	inflight sync.Map // synthHash -> struct{}
}

func (g *Grabber) Run(ctx context.Context) {
	interval := g.PollInterval
	if interval == 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.tick(ctx)
		}
	}
}

func (g *Grabber) tick(ctx context.Context) {
	for _, grab := range g.Store.List() {
		if grab.State == store.StateReady || grab.State == store.StateError {
			continue
		}
		if _, busy := g.inflight.LoadOrStore(grab.SynthHash, struct{}{}); busy {
			continue
		}
		go func(synthHash string) {
			defer g.inflight.Delete(synthHash)
			if err := g.advance(ctx, synthHash); err != nil {
				log.Printf("grab %s: %v", synthHash, err)
				_, _ = g.Store.Update(synthHash, func(gr *store.Grab) {
					gr.State = store.StateError
					gr.Error = err.Error()
				})
			}
		}(grab.SynthHash)
	}
}

// advance pushes one grab through its state machine. Called serially per grab.
func (g *Grabber) advance(ctx context.Context, synthHash string) error {
	grab, ok := g.Store.Get(synthHash)
	if !ok {
		return nil
	}

	// 1. Register with RD if not already.
	if grab.RDTorrentID == "" {
		// Reuse an RD torrent across grabs that share the same real infohash.
		var rdID string
		for _, sibling := range g.Store.ByRealHash(grab.RealHash) {
			if sibling.RDTorrentID != "" {
				rdID = sibling.RDTorrentID
				break
			}
		}
		if rdID == "" {
			res, err := g.RD.AddMagnet(ctx, grab.Magnet)
			if err != nil {
				return fmt.Errorf("rd addMagnet: %w", err)
			}
			rdID = res.ID
		}
		_, _ = g.Store.Update(synthHash, func(gr *store.Grab) {
			gr.RDTorrentID = rdID
			gr.State = store.StateDownloading
		})
		grab, _ = g.Store.Get(synthHash)
	}

	// 2. Probe RD state.
	info, err := g.RD.TorrentInfo(ctx, grab.RDTorrentID)
	if err != nil {
		return fmt.Errorf("rd torrentInfo: %w", err)
	}

	// 3. Ensure file selection covers all currently-known season needs across
	//    sibling grabs sharing this real infohash.
	if info.Status == "waiting_files_selection" || info.Status == "magnet_conversion" {
		if info.Status == "magnet_conversion" {
			return nil // try again next tick
		}
		fileIDs := g.fileIDsForGrabs(info.Files, g.Store.ByRealHash(grab.RealHash))
		if len(fileIDs) == 0 {
			// No file in the torrent matches any selected season — surface as error.
			return fmt.Errorf("no files in RD torrent match selected season(s)")
		}
		if err := g.RD.SelectFiles(ctx, grab.RDTorrentID, fileIDs); err != nil {
			return fmt.Errorf("rd selectFiles: %w", err)
		}
		return nil // poll again next tick
	}

	if info.Status != "downloaded" {
		// Still downloading on RD's side; update progress and wait.
		_, _ = g.Store.Update(synthHash, func(gr *store.Grab) {
			gr.TotalBytes = info.Bytes
			gr.DoneBytes = int64(info.Progress) * info.Bytes / 100
		})
		return nil
	}

	// 4. RD finished — fetch this grab's files to disk.
	if err := g.materialise(ctx, grab, info); err != nil {
		return fmt.Errorf("materialise: %w", err)
	}

	_, _ = g.Store.Update(synthHash, func(gr *store.Grab) {
		gr.State = store.StateReady
		gr.CompletedAt = time.Now().UTC()
		gr.DoneBytes = gr.TotalBytes
	})
	return nil
}

// fileIDsForGrabs returns the union of file IDs in `files` that belong to any
// grab's season. Used so a single selectFiles call covers all known siblings.
func (g *Grabber) fileIDsForGrabs(files []debrid.File, grabs []*store.Grab) []int {
	wantedSeasons := map[int]bool{}
	for _, gr := range grabs {
		wantedSeasons[gr.Season] = true
	}
	var ids []int
	for _, f := range files {
		s := seasonparse.FromFilename(f.Path)
		if s > 0 && wantedSeasons[s] {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// materialise downloads this grab's season's files into a content-addressed
// cache and hardlinks them into the grab's SavePath/<grab dir>/Season XX/.
// Sonarr is pointed at SavePath; the season subfolder is what it import-scans.
func (g *Grabber) materialise(ctx context.Context, grab *store.Grab, info *debrid.TorrentInfo) error {
	// Build a slice of (file, link) pairs by re-pairing info.Files with info.Links.
	// RD documents that links[] corresponds to files[] filtered to Selected==1,
	// in the same order.
	type pair struct {
		f    debrid.File
		link string
	}
	var pairs []pair
	li := 0
	for _, f := range info.Files {
		if f.Selected != 1 {
			continue
		}
		if li >= len(info.Links) {
			break
		}
		pairs = append(pairs, pair{f: f, link: info.Links[li]})
		li++
	}

	seasonDir := filepath.Join(grab.SavePath, fmt.Sprintf("Season %02d", grab.Season))
	// 0o755: shared with Sonarr's import scan; tightening to 0o750 breaks
	// setups where Sonarr runs as a different uid/gid than seasonsplitarr.
	if err := os.MkdirAll(seasonDir, 0o755); err != nil { // #nosec G301 -- shared-volume scenario
		return err
	}
	cacheRoot := filepath.Join(g.DownloadsDir, ".cache", strings.ToLower(grab.RealHash))
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil { // #nosec G301 -- shared-volume scenario
		return err
	}

	var totalBytes int64
	for _, p := range pairs {
		s := seasonparse.FromFilename(p.f.Path)
		if s != grab.Season {
			continue
		}
		base := filepath.Base(p.f.Path)
		cachePath := filepath.Join(cacheRoot, fmt.Sprintf("%d-%s", p.f.ID, base))
		linkPath := filepath.Join(seasonDir, base)

		if _, err := os.Stat(cachePath); os.IsNotExist(err) {
			if err := g.downloadOne(ctx, p.link, cachePath); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}

		// Hardlink (fast, zero-copy); fall back to symlink across devices.
		_ = os.Remove(linkPath)
		if err := os.Link(cachePath, linkPath); err != nil {
			if err := os.Symlink(cachePath, linkPath); err != nil {
				return fmt.Errorf("link %s -> %s: %w", linkPath, cachePath, err)
			}
		}
		totalBytes += p.f.Bytes
	}

	_, _ = g.Store.Update(grab.SynthHash, func(gr *store.Grab) {
		gr.ContentPath = seasonDir
		gr.TotalBytes = totalBytes
	})
	return nil
}

func (g *Grabber) downloadOne(ctx context.Context, restrictedLink, dst string) error {
	u, err := g.RD.Unrestrict(ctx, restrictedLink)
	if err != nil {
		return fmt.Errorf("unrestrict: %w", err)
	}
	tmp := dst + ".part"
	// dst is built from a validated infohash + filepath.Base of an RD path
	// (no traversal possible), under a directory we created.
	f, err := os.Create(tmp) // #nosec G304 -- path components are sanitised upstream
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := g.RD.DownloadFile(ctx, u.Download, f); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// Compile-time check that io is used (avoids unused import on refactors).
var _ = io.Discard
