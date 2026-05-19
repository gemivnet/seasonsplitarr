# seasonsplitarr

> Make Sonarr handle multi-season torrent packs — without changing Sonarr.

> [!IMPORTANT]
> **Disclaimer.** seasonsplitarr is an AI-assisted personal project, built
> to solve one user's specific problem and shared publicly in case it's
> useful. It comes with **no warranty and no liability** — for security,
> data loss, Real-Debrid account issues, missed episodes, blocklisted
> indexers, or anything else. Use at your own risk.
>
> Contributions are welcome but accepted at the maintainer's discretion
> and bandwidth, and there is no guarantee that issues will be addressed
> or that the project will continue to be maintained. If that's a
> blocker for you, please **fork the repo** in accordance with the
> [AGPL-3.0 license](./LICENSE) and adapt it to your needs.
>
> Security issues: report however you prefer — see
> [SECURITY.md](./SECURITY.md) for options. No SLA, no commitment.

Sonarr can't import a torrent that contains multiple seasons in one release
(e.g. `Some.Show.S01-S07.COMPLETE.1080p...`). It will grab only the first
season's metadata, get confused by the rest, and often blocklist the release.
This is a [long-standing limitation](https://github.com/Sonarr/Sonarr/issues/1007)
the Sonarr team has explicitly chosen not to fix.

**seasonsplitarr** is a small Go service that sits between Sonarr, your
indexers, and Real-Debrid. To Sonarr it looks like a normal Torznab indexer
and a normal qBittorrent download client. Behind the scenes it:

1. **Splits multi-season search results** — when an indexer returns
   `Show.S01-S07.COMPLETE...`, seasonsplitarr fans it out into 7 per-season
   results (`Show.S01...` through `Show.S07...`) that Sonarr can grab one at
   a time.
2. **Manages the grab end-to-end** — when Sonarr "downloads" a synthetic
   season release, seasonsplitarr adds the underlying torrent to Real-Debrid
   exactly once and presents per-season folders to Sonarr containing only
   that season's files.

From Sonarr's point of view: it asked for a season, it got a season. Done.

---

## Is this for you?

### ✅ seasonsplitarr is for you if…
- You run **Sonarr** and want it to handle multi-season packs natively.
- You use **Real-Debrid** (typically via [RDT-Client](https://github.com/rogerfar/rdt-client)
  or similar) for torrents.
- You manage indexers through **Prowlarr**.
- You're comfortable adding one indexer and one download client to your stack.

### ❌ seasonsplitarr is **not** for you if…
- You **don't use Real-Debrid** — the v0.x download path only supports RD.
  Native qBittorrent / Transmission / Deluge support is out of scope for now;
  the per-file selection semantics needed to make raw torrents work without
  Sonarr-side confusion are messy ([details](#why-real-debrid-only-for-now)).
- You use **Usenet** — Sonarr already handles NZB-based season packs more
  gracefully than torrents, and the failure modes are different. seasonsplitarr
  is torrent-side only.
- You're looking for a **post-processing splitter** (merge already-downloaded
  episodes into a season pack) — check out
  [seasonpackarr](https://github.com/nuxencs/seasonpackarr), which solves the
  opposite problem.
- You want **Radarr** support — movies don't have a multi-season problem.
  seasonsplitarr is Sonarr-only.

---

## How it works

```
        ┌─────────────────────────────────┐
Sonarr →│ seasonsplitarr                  │→ Prowlarr (upstream Torznab)
        │  • /torznab    (indexer surface)│
        │  • /api/v2     (qBittorrent API)│→ Real-Debrid (download)
        └─────────────────────────────────┘
                       │
                       └→ /downloads/seasonsplitarr/<grab-id>/Season XX/
                              ↑ Sonarr import-scans here
```

- **Indexer side:** seasonsplitarr proxies Torznab search to your upstream
  (usually Prowlarr), parses titles, and synthesises per-season results for
  any multi-season pack it sees. Synthetic GUIDs are deterministic, so
  re-searches return the same release identity.
- **Download side:** seasonsplitarr speaks just enough of the qBittorrent
  WebUI API to be added as a download client in Sonarr. When Sonarr "grabs"
  a synthetic season release, seasonsplitarr maps it back to the underlying
  infohash, adds it to Real-Debrid once, and exposes a season-scoped folder
  that contains only that season's files (via symlinks).

Sonarr's import scan only ever sees the season it asked for.

---

## Status

**Pre-alpha.** The Torznab proxy + season splitter is implemented and tested.
The qBittorrent API shim is scaffolded but the Real-Debrid integration and
symlink presenter are still stubs. **Do not yet point a live Sonarr at this.**

Track progress on the [issues page](https://github.com/gemivnet/seasonsplitarr/issues).

---

## Setup

Setup is **three steps total**: run the container, add one indexer in
Prowlarr, add one download client in Sonarr. All seasonsplitarr config lives
in your `docker-compose.yml` — there is no config file.

### 1. Run seasonsplitarr

Copy [`docker-compose.example.yml`](./docker-compose.example.yml) to
`docker-compose.yml` and fill in the four env vars:

| Variable | What |
|---|---|
| `SS_UPSTREAM_URL` | One or more Torznab feed URLs, comma-separated. From Prowlarr → Indexers → click an indexer → **Copy Torznab Feed**. Searches fan out across all of them in parallel and merge with infohash dedupe. |
| `SS_UPSTREAM_APIKEY` | API key for those upstreams. Prowlarr uses one key for all indexers, so a single value works; supply a comma-separated list if upstreams need different keys. |
| `SS_APIKEY` | Random string ≥ 16 chars (`openssl rand -hex 24`). You'll paste this into Prowlarr in step 2. |
| `SS_QBIT_USERNAME` | Username Sonarr will use to log in to seasonsplitarr's download-client interface. Anything you want. |
| `SS_QBIT_PASSWORD` | Strong password ≥ 12 chars (`openssl rand -base64 18`). Without this, anyone on your network could submit magnets to seasonsplitarr. |
| `SS_REALDEBRID_TOKEN` | From <https://real-debrid.com/apitoken>. |

Then:
```bash
docker compose up -d
```

### 2. Add seasonsplitarr to Prowlarr (indexer side)

In Prowlarr → **Indexers** → **Add Indexer** → **Generic Torznab**:
- **URL:** `http://seasonsplitarr:7474/torznab`
- **API Key:** the random string you set as `SS_APIKEY`
- **Categories:** TV (5000)

Prowlarr will sync this to Sonarr automatically — no Sonarr-side indexer
config needed.

### 3. Add seasonsplitarr to Sonarr (download client side)

In Sonarr → **Settings → Download Clients** → **Add** → **qBittorrent**:
- **Host:** `seasonsplitarr`
- **Port:** `7474`
- **Username/Password:** match `SS_QBIT_USERNAME` / `SS_QBIT_PASSWORD`
- **Category:** `tv-sonarr` (or whatever you use)

That's it. Do a manual search in Sonarr — multi-season packs from your
indexers should now appear as one result per season.

---

## Development

```bash
go test ./...
go build ./cmd/seasonsplitarr
SS_UPSTREAM_URL=... SS_APIKEY=... ./seasonsplitarr
```

Project layout:
```
cmd/seasonsplitarr/      # main entrypoint
internal/torznab/        # Torznab proxy + season-range detection
internal/qbittorrent/    # qBit WebUI API shim
internal/config/         # YAML config loader
```

The detection regexes live in `internal/torznab/splitter.go`. If you find a
release-naming convention that isn't being detected, a PR adding a test case
and a pattern is very welcome.

---

## Why Real-Debrid only (for now)?

With a regular torrent client (qBittorrent, Transmission, Deluge),
seasonsplitarr would need to advertise N magnet links pointing at the same
infohash. Real torrent clients dedupe by infohash, so Sonarr's 5 "grabs"
collapse into a single download, and the other 4 sit in queue waiting for
files that already exist — they typically get marked as failed and the
release gets blocklisted.

Real-Debrid sidesteps this entirely: RD itself dedupes the underlying
torrent, and seasonsplitarr is free to present 5 distinct "completed
downloads" backed by 5 different file subsets of the same RD torrent.

Adding native-torrent-client support is possible but requires intercepting
the download client's per-file selection (e.g. via qBit's category-execute
hooks) and managing partial-file state. It's a significant additional
project and not on the v0.x roadmap.

---

## Related projects

- [seasonpackarr](https://github.com/nuxencs/seasonpackarr) — opposite
  direction: merges already-downloaded episodes into a season-pack folder.
- [Decypharr](https://github.com/sirrobot01/decypharr) — Real-Debrid library
  presenter; per-season symlinking after download.
- [Prowlarr](https://github.com/Prowlarr/Prowlarr) — the indexer aggregator
  seasonsplitarr proxies.

---

## License

[AGPL-3.0](./LICENSE)
