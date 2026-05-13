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

type Config struct {
	Listen          string
	UpstreamURL     string
	UpstreamAPIKey  string
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
}

func Load() (*Config, error) {
	c := &Config{
		Listen:          getenv("SS_LISTEN", "0.0.0.0:7474"),
		UpstreamURL:     os.Getenv("SS_UPSTREAM_URL"),
		UpstreamAPIKey:  os.Getenv("SS_UPSTREAM_APIKEY"),
		APIKey:          os.Getenv("SS_APIKEY"),
		RealDebridToken: os.Getenv("SS_REALDEBRID_TOKEN"),
		DownloadsDir:    getenv("SS_DOWNLOADS_DIR", "/downloads/seasonsplitarr"),
		QBitUsername:    os.Getenv("SS_QBIT_USERNAME"),
		QBitPassword:    os.Getenv("SS_QBIT_PASSWORD"),
	}

	var missing []string
	if c.UpstreamURL == "" {
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
	return c, nil
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
