# Security

## Reporting a vulnerability

seasonsplitarr is a personal project shared publicly. You're welcome to
report security issues however you prefer — there are no rules here:

- [GitHub Security Advisories](https://github.com/gemivnet/seasonsplitarr/security/advisories/new) if you want to coordinate privately first.
- A regular [public issue](https://github.com/gemivnet/seasonsplitarr/issues) is also fine.
- Public disclosure (Reddit, blog, etc.) is your call. I'd ask for a heads-up if it's practical, but you don't owe me one.

I'll do my best to fix what comes in, but **no commitment** on response
time or whether a fix ships at all. If something's blocking you, the
[license](./LICENSE) lets you fork.

## Threat model

seasonsplitarr is designed to run on a trusted home network alongside the
rest of your *arr stack. Specifically, we assume:

- The host running seasonsplitarr is **not** directly exposed to the
  internet. If you need remote access, put it behind a reverse proxy
  that handles TLS and additional access control (Authelia, Cloudflare
  Access, a VPN, etc.).
- Other services on the same Docker network (Sonarr, Prowlarr) are
  trusted. seasonsplitarr does not authenticate inter-container traffic
  beyond what's described below.
- The user running the container has read/write access to
  `SS_DOWNLOADS_DIR`. Files written there are world-readable inside the
  container by default; tighten via volume mount options if needed.

## Auth surfaces

| Surface | Endpoint prefix | Auth |
|---|---|---|
| Torznab proxy | `/torznab/*` | `apikey` query param compared with `SS_APIKEY` in constant time. Required. |
| qBittorrent shim | `/api/v2/*` | Session cookie obtained via `/api/v2/auth/login` with `SS_QBIT_USERNAME` / `SS_QBIT_PASSWORD`. Required on every endpoint except `auth/login` and `auth/logout`. |
| Health | `/healthz` | None. Returns the literal string `ok`. No state exposed. |

The startup config check rejects empty or short credentials:
- `SS_APIKEY` must be ≥ 16 characters.
- `SS_QBIT_PASSWORD` must be ≥ 12 characters.

There is no admin/setup endpoint, no settings API, no token-issuance
endpoint, no remote-config writer. The only state-changing endpoints are
`torrents/add` and `torrents/delete`, and both require the SID cookie.

## What we deliberately don't do

We learned from the Huntarr writeup ([May 2026 post](https://www.reddit.com/r/huntarr/))
and chose a deliberately small surface area:

- **No web UI.** Every additional rendered page is another XSS / CSRF
  surface. State is in `.state.json`; debugging is `cat` and `jq`.
- **No settings-API.** Configuration is env vars only; the running
  process has no endpoint that can change its own behavior.
- **No "owner account" / multi-user model.** One static credential pair
  for the qBit shim, one static API key for Torznab. Rotate by changing
  env vars and restarting.
- **No unauthenticated state mutation.** Every endpoint that touches the
  store requires the SID cookie.
- **No reflection of secrets.** API responses don't include `SS_APIKEY`,
  `SS_REALDEBRID_TOKEN`, or `SS_UPSTREAM_APIKEY` — even to authenticated
  callers. `app/preferences` returns paths and feature flags only.

## Known limitations / non-goals

These are documented gaps, not undiscovered bugs:

- **No brute-force protection on `/api/v2/auth/login`.** Rely on the
  password's randomness. (Future: per-IP backoff.)
- **No TLS.** Run behind a reverse proxy if you need it.
- **No CSRF tokens.** Sonarr doesn't use a browser, so SameSite=Strict
  on the SID cookie plus origin-isolation in your reverse-proxy is the
  defense. Don't load the UI of any qBit-compatible client in the same
  browser session you use for sensitive sites.
- **`SS_UPSTREAM_APIKEY` is forwarded as a query param** to the upstream
  Torznab server. If you don't trust the network path to Prowlarr, route
  it through localhost or a private Docker network.
- **State file at `${SS_DOWNLOADS_DIR}/.state.json` (mode 0600).** Mount
  this path appropriately — anything that can read the container's
  filesystem can read magnet URLs and RD torrent IDs (not the RD token).
- **The Real-Debrid token (`SS_REALDEBRID_TOKEN`) is held in process
  memory only.** It's never written to disk, never returned in any API
  response, and never logged. If a process dump leaks, the token leaks.
- **Direct-download URLs from Real-Debrid** are time-limited and used
  internally. They are never logged.

## What we'd accept as a PR

- Per-IP rate limiting on `/api/v2/auth/login`.
- A `SECURITY_CONTACT` GHSA flow doc once we have a maintainer email.
- Static analysis (gosec, govulncheck) in CI.
- Tests for the qBit shim auth flow (specifically: every non-`auth/*`
  endpoint returns 403 without a valid SID).

## What we won't accept as a PR

- A web admin UI.
- Endpoints that mutate config at runtime.
- Endpoints that return any value of `SS_*`.
- "Sonarr-like" hot-reload of credentials without a process restart.

The smaller the surface, the easier the next review.
