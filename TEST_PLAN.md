# seasonsplitarr — internal test plan

> Working doc, not for end users. Lists the validations we run before
> tagging a release. We move through the phases in order — don't graduate
> to the next phase until the previous one is green.

## Phase 0 — Unit tests

```bash
go test ./...
```

Currently covers:
- `internal/torznab`: title pattern detection, deterministic synthetic
  infohashes, synthetic title rewriting.
- `internal/seasonparse`: per-file season extraction (SxxEyy, NxNN,
  "Season N", "Series N" folders).
- `internal/store`: JSON persistence round-trip, lookup by real-hash.

**Gaps to close:** no unit tests yet for `internal/qbittorrent` (the qBit
shim handlers) or for `internal/torznab/proxy.go` (the splitFeed XML
rewriting). Both are HTTP-shaped — add httptest-driven tests.

## Phase 1 — Local smoke test (no Sonarr, no real RD)

Run the binary against a canned Torznab response and probe with `curl`.
The goal is to validate the shapes Sonarr and Prowlarr will see before we
let live software near it.

### 1a. Fixture server
Stand up a tiny Go HTTP server (in `cmd/fixture-torznab/`, not yet
written) that serves a hardcoded RSS feed containing one multi-season
release. Suggested fixture title: `My.Show.S01-S03.COMPLETE.1080p.x264-FIXTURE`
with a fake magnet of `magnet:?xt=urn:btih:0000000000000000000000000000000000000001&dn=My.Show.S01-S03.COMPLETE`.

### 1b. Run seasonsplitarr against the fixture
```bash
SS_UPSTREAM_URL=http://localhost:9999/api \
SS_UPSTREAM_APIKEY=fixture-key \
SS_APIKEY=test-apikey \
SS_REALDEBRID_TOKEN=unused-in-this-phase \
SS_DOWNLOADS_DIR=$(mktemp -d) \
SS_LISTEN=127.0.0.1:7474 \
./seasonsplitarr
```

### 1c. Validate the search response

```bash
curl -s "http://127.0.0.1:7474/torznab/api?t=tvsearch&apikey=test-apikey" | xmllint --format -
```

**Expect:**
- 3 `<item>` elements (one per season).
- Titles `My.Show.S01...`, `My.Show.S02...`, `My.Show.S03...`.
- Three distinct `<guid>` values, each starting `seasonsplitarr-`.
- Each `enclosure.url` contains its own `xt=urn:btih:<hash>` and the
  three hashes are distinct from each other and from the original.
- `<torznab:attr name="infohash" ...>` values match the per-item hash.

### 1d. Validate the qBit shim surface

```bash
# Sonarr's first call after configuration:
curl -s http://127.0.0.1:7474/api/v2/app/version           # -> v4.6.0
curl -s http://127.0.0.1:7474/api/v2/app/webapiVersion     # -> 2.9.3
curl -s http://127.0.0.1:7474/api/v2/app/preferences | jq

# Login (Sonarr POSTs username/password):
curl -s -X POST -d "username=x&password=y" \
  http://127.0.0.1:7474/api/v2/auth/login                  # -> Ok.

# Add a magnet (simulating a grab):
SYNTH=$(curl -s "http://127.0.0.1:7474/torznab/api?t=tvsearch&apikey=test-apikey" \
  | xmllint --xpath 'string(//item[2]/enclosure/@url)' -)
echo "Will grab S02 magnet: $SYNTH"
curl -s -X POST -F "urls=$SYNTH" -F "category=tv-sonarr" \
  http://127.0.0.1:7474/api/v2/torrents/add                # -> Ok.

# Confirm it appears in torrents/info:
curl -s "http://127.0.0.1:7474/api/v2/torrents/info?category=tv-sonarr" | jq
```

**Expect:** one torrent with `name` matching `My.Show.S02...`, `hash`
matching the synthetic infohash, `category` `tv-sonarr`, `state`
`queuedDL` (RD never reaches "downloaded" in this phase because we have
no token — that's expected). Verify `save_path` is under
`SS_DOWNLOADS_DIR`.

**Known fail mode:** the grabber logs an RD error every poll interval.
That's expected in phase 1. The store entry should remain even after errors.

### 1e. Persistence round-trip
Kill seasonsplitarr (Ctrl-C), restart with the same env, hit
`torrents/info` again. The S02 grab should still be there with the same
hash and timestamps.

## Phase 2 — Add Sonarr + Prowlarr (still no real RD)

Spin up the full *arr stack with disposable containers. Goal: verify
Sonarr accepts seasonsplitarr as both an indexer (via Prowlarr) and a
download client, and that manual search shows split seasons.

### Stack
Use the existing `docker-compose.example.yml` plus disposable Sonarr +
Prowlarr containers on the same network. Bind-mount everything in `tmp/`
so it's easy to wipe between runs.

### Steps
1. Add seasonsplitarr to Prowlarr → Indexers → Generic Torznab.
   - URL: `http://seasonsplitarr:7474/torznab`
   - API key: matches `SS_APIKEY`.
   - **Pass condition:** Prowlarr's "Test" succeeds; the indexer syncs to
     Sonarr automatically.
2. In Sonarr → Indexers, confirm the seasonsplitarr indexer is listed
   and `Test` passes.
3. Add a fictitious series in Sonarr that matches the fixture title
   (`My.Show`). Wait for the initial search, or do a manual interactive
   search.
   - **Pass condition:** Sonarr's manual search lists three results
     (one per season) attributed to the seasonsplitarr indexer.
4. Add seasonsplitarr to Sonarr → Settings → Download Clients → qBittorrent.
   - Host: `seasonsplitarr`, Port: 7474, anything for username/password,
     Category: `tv-sonarr`.
   - **Pass condition:** Sonarr's "Test" succeeds.
5. Grab one of the synthetic releases from manual search.
   - **Pass condition:** Sonarr reports the grab as Queued/Downloading
     (will stay in that state — RD isn't real yet). No errors about
     duplicate hash, blocklist, etc.

## Phase 3 — Real Real-Debrid, single magnet

Use a real RD token and a small known-good multi-season release.

### Choosing a test release
- Pick a multi-season pack that's *cached* on RD already (instant) so we
  don't wait on the RD downloader. Use a public-domain title (e.g. an
  old BBC series) to keep this above board.
- Confirm via RD's own UI that the infohash is cached and the file list
  contains per-season folders or `SxxEyy`-named episodes.

### Steps
1. Submit the test magnet manually to the fixture so seasonsplitarr
   sees it via `tvsearch`.
2. Grab season N from Sonarr.
3. Watch the grabber log progress through:
   `queued → downloading → ready`. Track in
   `${SS_DOWNLOADS_DIR}/.state.json`.
4. **Pass conditions:**
   - `${SS_DOWNLOADS_DIR}/.cache/<realhash>/` contains the season's
     files.
   - `${SS_DOWNLOADS_DIR}/<synthHash>/Season NN/` contains hardlinks to
     them.
   - Sonarr's queue shows the grab as completed and import succeeds:
     episodes show as monitored/owned, files end up in the series root
     folder.
   - **Critically:** Sonarr does NOT report import errors about extra
     episodes — only season N should be visible to it.

## Phase 4 — Multiple seasons of the same pack

Grab season 1 and season 2 of the same pack back-to-back.

**Pass conditions:**
- Only one RD torrent is created (`grab1.RDTorrentID == grab2.RDTorrentID`
  in `.state.json`).
- Files are downloaded from RD only once per file (verify via
  `ls -i ${SS_DOWNLOADS_DIR}/.cache/...` — same inode reused across grabs
  via hardlinks).
- Both seasons import into Sonarr cleanly.

## Phase 5 — Failure modes (manual, eyeball)

Each of these should NOT brick the service. Verify the affected grab
ends up in `state: error` with a human-readable `error` field and other
grabs continue to progress.

- **Invalid magnet:** post a malformed magnet to `torrents/add`.
- **RD token revoked:** rotate the RD token mid-flight; expect 401 errors
  in `error` field.
- **Uncached infohash on RD:** grab a magnet RD hasn't cached. Should
  stay in `downloading` while RD fetches, then proceed normally. Time
  out only if RD itself times out.
- **Season folder pre-exists:** grab the same season twice. Second grab
  should idempotently re-link without error.
- **DiskFull on cache write:** fill `${SS_DOWNLOADS_DIR}` to 100%, grab.
  Expect a clear `error` with no partial files left behind.

## Phase 6 — Live use (canary)

Point a personal Sonarr instance at seasonsplitarr alongside the existing
download client. Grab one real series with multi-season packs. Watch for
two weeks before promoting to default.

Things to watch in the canary:
- Sonarr blocklist size — should not grow due to seasonsplitarr grabs.
- Disk usage of `${SS_DOWNLOADS_DIR}/.cache/` — needs a TTL/GC story
  before we go wide.
- Whether Sonarr ever asks for the same `synthHash` twice (e.g. after a
  failed import) — we should accept and re-link rather than 409.

## Out of scope for v0.x

- Native qBittorrent client (non-RD) support.
- Radarr.
- Usenet.
- Cache eviction / disk pressure.
- Rate limiting against the upstream Torznab provider.
- Authentication beyond the static `SS_APIKEY` (e.g. per-app tokens).
- Web UI.
