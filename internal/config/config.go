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
	Listen           string
	UpstreamURL      string
	UpstreamAPIKey   string
	APIKey           string
	RealDebridToken  string
	DownloadsDir     string
}

func Load() (*Config, error) {
	c := &Config{
		Listen:          getenv("SS_LISTEN", "0.0.0.0:7474"),
		UpstreamURL:     os.Getenv("SS_UPSTREAM_URL"),
		UpstreamAPIKey:  os.Getenv("SS_UPSTREAM_APIKEY"),
		APIKey:          os.Getenv("SS_APIKEY"),
		RealDebridToken: os.Getenv("SS_REALDEBRID_TOKEN"),
		DownloadsDir:    getenv("SS_DOWNLOADS_DIR", "/downloads/seasonsplitarr"),
	}

	var missing []string
	if c.UpstreamURL == "" {
		missing = append(missing, "SS_UPSTREAM_URL")
	}
	if c.APIKey == "" {
		missing = append(missing, "SS_APIKEY")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
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
