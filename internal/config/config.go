// Package config loads seasonsplitarr's runtime configuration from
// environment variables. All settings use the SS_ prefix so they can't
// collide with other *arr-stack env vars in a shared compose file.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Upstream is one Torznab indexer that seasonsplitarr proxies.
type Upstream struct {
	URL    string
	APIKey string
}

type Config struct {
	Listen string
	// Upstreams is the list of Torznab indexers to fan out to. Order matches
	// SS_UPSTREAM_URL / SS_UPSTREAM_APIKEY entries.
	Upstreams       []Upstream
	APIKey          string
	RealDebridToken string
	DownloadsDir    string

	// QBitUsername / QBitPassword gate the qBittorrent-compatible download
	// client surface. They are REQUIRED — Sonarr will log in with them, and
	// every other request on /api/v2/* requires the resulting SID cookie.
	// Without these set, any host on the network could submit magnets and
	// read grab state.
	QBitUsername string
	QBitPassword string

	// SonarrURL / SonarrAPIKey are OPTIONAL. When set, the grabber will
	// reach back into Sonarr via /api/v3/queue when a grab fails
	// irrecoverably (e.g. RD 451 infringing_file) and ask Sonarr to
	// blocklist the release and immediately re-search. Without this,
	// permanently-failed grabs sit in the queue with a "qBittorrent is
	// reporting an error" warning and require manual cleanup, because the
	// qBit protocol has no way to signal "failed, please try again"
	// (Sonarr maps state=error → Warning by design).
	SonarrURL    string
	SonarrAPIKey string
}

func Load() (*Config, error) {
	c := &Config{
		Listen:          getenv("SS_LISTEN", "0.0.0.0:7474"),
		APIKey:          os.Getenv("SS_APIKEY"),
		RealDebridToken: os.Getenv("SS_REALDEBRID_TOKEN"),
		DownloadsDir:    getenv("SS_DOWNLOADS_DIR", "/downloads/seasonsplitarr"),
		QBitUsername:    os.Getenv("SS_QBIT_USERNAME"),
		QBitPassword:    os.Getenv("SS_QBIT_PASSWORD"),
		SonarrURL:       os.Getenv("SS_SONARR_URL"),
		SonarrAPIKey:    os.Getenv("SS_SONARR_APIKEY"),
	}

	urls := splitCSV(os.Getenv("SS_UPSTREAM_URL"))
	keys := splitCSV(os.Getenv("SS_UPSTREAM_APIKEY"))

	var missing []string
	if len(urls) == 0 {
		missing = append(missing, "SS_UPSTREAM_URL")
	}
	if c.APIKey == "" {
		missing = append(missing, "SS_APIKEY")
	}
	if c.QBitUsername == "" {
		missing = append(missing, "SS_QBIT_USERNAME")
	}
	if c.QBitPassword == "" {
		missing = append(missing, "SS_QBIT_PASSWORD")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	if len(c.APIKey) < 16 {
		return nil, fmt.Errorf("SS_APIKEY must be at least 16 characters")
	}
	if len(c.QBitPassword) < 12 {
		return nil, fmt.Errorf("SS_QBIT_PASSWORD must be at least 12 characters")
	}

	// Pair URLs with keys positionally. If only one key is supplied, reuse it
	// for every URL — common when proxying multiple Prowlarr indexers that
	// share the same Prowlarr API key.
	if len(keys) > 1 && len(keys) != len(urls) {
		return nil, fmt.Errorf("SS_UPSTREAM_APIKEY has %d entries but SS_UPSTREAM_URL has %d; supply one key, or one per URL",
			len(keys), len(urls))
	}
	for i, u := range urls {
		var k string
		switch len(keys) {
		case 0:
			k = ""
		case 1:
			k = keys[0]
		default:
			k = keys[i]
		}
		c.Upstreams = append(c.Upstreams, Upstream{URL: u, APIKey: k})
	}
	return c, nil
}

// splitCSV splits a comma-separated env value, trims whitespace, and drops empties.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ErrIncomplete is returned when required vars are missing. Kept exported in
// case callers want to distinguish startup errors.
var ErrIncomplete = errors.New("incomplete config")
