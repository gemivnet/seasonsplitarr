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
	"github.com/gemivnet/seasonsplitarr/internal/logging"
	"github.com/gemivnet/seasonsplitarr/internal/seasonparse"
	"github.com/gemivnet/seasonsplitarr/internal/sonarr"
	"github.com/gemivnet/seasonsplitarr/internal/store"
)

var glog = logging.New("grabber")

type Grabber struct {
	Store        *store.Store
	RD           *debrid.Client
	// Sonarr is optional. When non-nil, permanently-failed grabs (e.g. RD
	// 451 infringing_file) are reported to Sonarr via /api/v3/queue so it
	// blocklists the release and immediately retries with the next-best
	// candidate. When nil, those grabs are just marked StateError and sit
	// in the queue until manually cleared.
	Sonarr       *sonarr.Client
	DownloadsDir string
	// PollInterval controls how often the grabber scans the store for work.
	PollInterval time.Duration
	// inflight prevents two concurrent ticks from operating on the same grab.
	inflight sync.Map // synthHash -> struct{}
	// rdAddLocks serialises addMagnet calls per realHash so sibling synthetic
	// grabs share one RD torrent instead of racing to create N duplicates and
	// tripping RD's per-account rate limit.
	rdAddLocks sync.Map // realHash -> *sync.Mutex
}

func (g *Grabber) lockRealHash(realHash string) *sync.Mutex {
	v, _ := g.rdAddLocks.LoadOrStore(realHash, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m
}

func (g *Grabber) Run(ctx context.Context) {
	interval := g.PollInterval
	if interval == 0 {
		interval = 10 * time.Second
	}
	glog.Info("grabber loop starting (poll interval=%s, downloads=%s)", interval, g.DownloadsDir)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			glog.Info("grabber loop stopping (context cancelled)")
			return
		case <-t.C:
			g.tick(ctx)
		}
	}
}

func (g *Grabber) tick(ctx context.Context) {
	all := g.Store.List()
	active := 0
	for _, grab := range all {
		if grab.State == store.StateReady || grab.State == store.StateError || grab.State == store.StateRegistered {
			continue
		}
		active++
		if _, busy := g.inflight.LoadOrStore(grab.SynthHash, struct{}{}); busy {
			glog.Debug("tick: %s still inflight, skipping", grab.SynthHash[:8])
			continue
		}
		glog.Debug("tick: advancing %s (state=%s title=%q)", grab.SynthHash[:8], grab.State, grab.Title)
		go func(synthHash string) {
			defer g.inflight.Delete(synthHash)
			if err := g.advance(ctx, synthHash); err != nil {
				glog.Error("grab %s: %v", synthHash, err)
				log.Printf("grab %s: %v", synthHash, err)
				_, _ = g.Store.Update(synthHash, func(gr *store.Grab) {
					gr.State = store.StateError
					gr.Error = err.Error()
				})
				// If the failure is permanent (e.g. RD 451 infringing_file)
				// and Sonarr's API is configured, ask Sonarr to blocklist
				// the release and retry with the next candidate. Sonarr's
				// own qBit-protocol view maps state=error to Warning by
				// design, so without this out-of-band call these grabs
				// just pile up in the queue forever.
				if isPermanentFailure(err) {
					g.handlePermanentFailure(ctx, synthHash, err)
				}
			}
		}(grab.SynthHash)
	}
	if active == 0 && len(all) > 0 {
		glog.Debug("tick: %d total grabs, none active", len(all))
	}
}

// advance pushes one grab through its state machine. Called serially per grab.
func (g *Grabber) advance(ctx context.Context, synthHash string) error {
	grab, ok := g.Store.Get(synthHash)
	if !ok {
		return nil
	}

	// 1. Register with RD if not already. Serialise per realHash so a pack
	//    that spawned N synthetic siblings only hits addMagnet once.
	if grab.RDTorrentID == "" {
		mu := g.lockRealHash(grab.RealHash)
		// Re-check store under the lock — a sibling may have set RDTorrentID
		// while we were waiting.
		current, _ := g.Store.Get(synthHash)
		if current != nil && current.RDTorrentID != "" {
			mu.Unlock()
			grab = current
		} else {
			var rdID string
			for _, sibling := range g.Store.ByRealHash(grab.RealHash) {
				if sibling.RDTorrentID != "" {
					rdID = sibling.RDTorrentID
					glog.Info("advance %s: reusing RD torrent %s from sibling S%02d",
						synthHash[:8], rdID, sibling.Season)
					break
				}
			}
			if rdID == "" {
				glog.Info("advance %s: adding magnet to Real-Debrid (S%02d %q)",
					synthHash[:8], grab.Season, grab.Title)
				res, err := g.RD.AddMagnet(ctx, grab.Magnet)
				if err != nil {
					mu.Unlock()
					return fmt.Errorf("rd addMagnet: %w", err)
				}
				rdID = res.ID
				glog.Info("advance %s: RD torrent created, id=%s", synthHash[:8], rdID)
			}
			_, _ = g.Store.Update(synthHash, func(gr *store.Grab) {
				gr.RDTorrentID = rdID
				gr.State = store.StateDownloading
			})
			mu.Unlock()
			grab, _ = g.Store.Get(synthHash)
		}
	}

	// 2. Probe RD state.
	info, err := g.RD.TorrentInfo(ctx, grab.RDTorrentID)
	if err != nil {
		return fmt.Errorf("rd torrentInfo: %w", err)
	}
	glog.Debug("advance %s: RD status=%s progress=%.1f%% files=%d links=%d",
		synthHash[:8], info.Status, info.Progress, len(info.Files), len(info.Links))

	// 3. Ensure file selection covers all currently-known season needs across
	//    sibling grabs sharing this real infohash.
	if info.Status == "waiting_files_selection" || info.Status == "magnet_conversion" {
		if info.Status == "magnet_conversion" {
			glog.Debug("advance %s: RD still converting magnet, waiting", synthHash[:8])
			return nil // try again next tick
		}
		siblings := g.Store.ByRealHash(grab.RealHash)
		fileIDs := g.fileIDsForGrabs(info.Files, siblings)
		glog.Info("advance %s: selecting %d files for %d sibling grab(s)",
			synthHash[:8], len(fileIDs), len(siblings))
		if len(fileIDs) == 0 {
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
			gr.DoneBytes = int64(info.Progress * float64(info.Bytes) / 100)
		})
		return nil
	}

	// 4. RD finished — fetch this grab's files to disk.
	glog.Info("advance %s: RD ready, materialising season %d to disk", synthHash[:8], grab.Season)
	if err := g.materialise(ctx, grab, info); err != nil {
		return fmt.Errorf("materialise: %w", err)
	}

	_, _ = g.Store.Update(synthHash, func(gr *store.Grab) {
		gr.State = store.StateReady
		gr.CompletedAt = time.Now().UTC()
		gr.DoneBytes = gr.TotalBytes
	})
	glog.Info("advance %s: READY -> Sonarr can now import", synthHash[:8])
	return nil
}

// fileIDsForGrabs returns the union of file IDs in `files` that belong to any
// grab's season. If any sibling grab has season=0 (passthrough — Sonarr
// grabbed a single-episode release we didn't pre-register), we don't have
// season metadata to filter on, so select all files and let Sonarr's import
// scan pick what it wants.
func (g *Grabber) fileIDsForGrabs(files []debrid.File, grabs []*store.Grab) []int {
	wantedSeasons := map[int]bool{}
	hasPassthrough := false
	for _, gr := range grabs {
		if gr.Season == 0 {
			hasPassthrough = true
			break
		}
		wantedSeasons[gr.Season] = true
	}
	if hasPassthrough {
		ids := make([]int, 0, len(files))
		for _, f := range files {
			ids = append(ids, f.ID)
		}
		return ids
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
		// Synthetic season-split grabs filter by parsed season: the RD torrent
		// contains files from many seasons and we only want this one's. But
		// passthrough grabs (single-episode releases that weren't split) have
		// grab.Season=0 and the file parses to its real season, so the filter
		// would reject everything. Skip the filter when grab.Season==0.
		if grab.Season != 0 {
			s := seasonparse.FromFilename(p.f.Path)
			if s != grab.Season {
				continue
			}
		}
		base := filepath.Base(p.f.Path)
		cachePath := filepath.Join(cacheRoot, fmt.Sprintf("%d-%s", p.f.ID, base))
		linkPath := filepath.Join(seasonDir, base)

		if _, err := os.Stat(cachePath); os.IsNotExist(err) {
			glog.Info("download: %s (%d bytes)", base, p.f.Bytes)
			t0 := time.Now()
			if err := g.downloadOne(ctx, p.link, cachePath); err != nil {
				return err
			}
			glog.Info("download done: %s in %s", base, time.Since(t0))
		} else if err != nil {
			return err
		} else {
			glog.Debug("download skipped (cached): %s", base)
		}

		// Hardlink (fast, zero-copy); fall back to symlink across devices.
		_ = os.Remove(linkPath)
		if err := os.Link(cachePath, linkPath); err != nil {
			glog.Debug("hardlink failed (%v), trying symlink: %s -> %s", err, linkPath, cachePath)
			if err := os.Symlink(cachePath, linkPath); err != nil {
				return fmt.Errorf("link %s -> %s: %w", linkPath, cachePath, err)
			}
		} else {
			glog.Debug("hardlinked: %s -> %s", linkPath, cachePath)
		}
		totalBytes += p.f.Bytes
	}
	glog.Info("materialise %s: %d files, %d bytes total -> %s",
		grab.SynthHash[:8], len(pairs), totalBytes, seasonDir)

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

// isPermanentFailure reports whether an err from advance() represents a
// failure that retrying won't fix. Currently this is RD's content-blocking
// responses (451 infringing_file, 404 unknown_resource for a deleted/expired
// release, 403 permission_denied for region-locked content). These all need
// the same human-equivalent action: blocklist the release and try the next
// one.
//
// We detect by string content rather than typed errors because the RD client
// wraps errors with fmt.Errorf; refactoring it to expose status codes is a
// larger change for a check used in exactly one place.
func isPermanentFailure(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "infringing_file"):
		return true
	case strings.Contains(s, "unknown_resource"):
		return true
	case strings.Contains(s, "permission_denied"):
		return true
	}
	return false
}

// handlePermanentFailure asks Sonarr to remove the failed grab from its
// queue, blocklist the release, and immediately re-search for a replacement.
// On Sonarr-side success we also clear the grab from our own store so the
// qBit /torrents/info view doesn't keep advertising it (Sonarr would just
// log "removed from download client" and that's fine — Sonarr already knows
// it's gone because we just told it).
//
// Sonarr's queue is eventually consistent: it picks up grabs from /torrents/
// info polling, which has a multi-second lag. A grab can fail before Sonarr
// has noticed it. We retry the lookup a handful of times with backoff so
// brand-new grabs that 451 immediately still get blocklisted properly.
func (g *Grabber) handlePermanentFailure(ctx context.Context, synthHash string, failErr error) {
	if g.Sonarr == nil {
		glog.Debug("grab %s: permanent failure but Sonarr API not configured; leaving as StateError",
			synthHash[:8])
		return
	}
	const attempts = 5
	const backoff = 2 * time.Second
	var queueID int
	var found bool
	for i := 0; i < attempts; i++ {
		var err error
		queueID, found, err = g.Sonarr.FindByDownloadID(ctx, synthHash)
		if err != nil {
			glog.Warn("grab %s: Sonarr queue lookup failed (attempt %d/%d): %v",
				synthHash[:8], i+1, attempts, err)
			return // Sonarr unreachable; bail rather than retry blindly.
		}
		if found {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
	if !found {
		glog.Warn("grab %s: not found in Sonarr queue after %d attempts; leaving as StateError",
			synthHash[:8], attempts)
		return
	}
	if err := g.Sonarr.RemoveAndBlocklist(ctx, queueID); err != nil {
		glog.Warn("grab %s: Sonarr blocklist+remove (queueId=%d) failed: %v",
			synthHash[:8], queueID, err)
		return
	}
	glog.Info("grab %s: Sonarr blocklisted release (queueId=%d) — Sonarr will re-search. Reason: %v",
		synthHash[:8], queueID, failErr)
	// Sonarr will also DELETE on the qBit shim (with deleteFiles=true if
	// "Remove Completed" is on), which already removes the store entry and
	// any cached files. Belt-and-braces clear it here too in case the
	// /delete call doesn't follow for some reason — leaving stale Error
	// grabs in the store inflates the qBit torrents/info view forever.
	_ = g.Store.Delete(synthHash)
}
