// Package sonarr is a minimal Sonarr REST client. It exists solely so the
// grabber can drive Sonarr's "blocklist + retry" behavior when an RD grab
// fails permanently (e.g. 451 infringing_file). Sonarr's qBit protocol
// integration maps state=error → Warning (intentionally — see the comment
// on QBittorrent.cs `case "error"`), so there is no in-band way to make
// Sonarr blocklist a failed download. Out-of-band via /api/v3/queue is the
// only path that achieves auto-blocklist+immediate-retry.
package sonarr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gemivnet/seasonsplitarr/internal/logging"
)

var slog = logging.New("sonarr")

// Client is a thin wrapper around Sonarr's v3 API. Zero value is not usable —
// always construct via New().
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// New returns a Client, or nil if either input is empty. nil-receiver is
// safe for the methods on Client: callers check for nil instead of needing
// a separate "feature enabled" flag, since the only reason the client
// exists is the Sonarr-integration feature.
func New(baseURL, apiKey string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" || apiKey == "" {
		return nil
	}
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// queueRecord mirrors the fields seasonsplitarr cares about from
// /api/v3/queue records. Sonarr returns ~30 fields; the rest are ignored.
type queueRecord struct {
	ID         int    `json:"id"`
	DownloadID string `json:"downloadId"`
	Status     string `json:"status"`
	Title      string `json:"title"`
}

type queuePage struct {
	Page         int           `json:"page"`
	PageSize     int           `json:"pageSize"`
	TotalRecords int           `json:"totalRecords"`
	Records      []queueRecord `json:"records"`
}

// FindByDownloadID locates the Sonarr queue record whose downloadId matches
// the given hash (case-insensitive). Returns id, true if found.
//
// Sonarr's queue endpoint does not support a downloadId query parameter in
// v3, so we walk pages. The queue is typically small (active grabs only),
// so this is acceptable. To keep us bounded if it's not, we cap at 500
// records before giving up.
func (c *Client) FindByDownloadID(ctx context.Context, downloadID string) (int, bool, error) {
	if c == nil {
		return 0, false, nil
	}
	want := strings.ToLower(strings.TrimSpace(downloadID))
	if want == "" {
		return 0, false, fmt.Errorf("empty downloadId")
	}
	const pageSize = 100
	const maxRecords = 500
	seen := 0
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", fmt.Sprintf("%d", page))
		q.Set("pageSize", fmt.Sprintf("%d", pageSize))
		// includeUnknownSeriesItems=true so we still see grabs that aren't yet
		// attached to a known series (rare, but happens during series-add races).
		q.Set("includeUnknownSeriesItems", "true")
		var pg queuePage
		if err := c.get(ctx, "/api/v3/queue?"+q.Encode(), &pg); err != nil {
			return 0, false, err
		}
		for _, r := range pg.Records {
			if strings.EqualFold(r.DownloadID, want) {
				return r.ID, true, nil
			}
		}
		seen += len(pg.Records)
		if len(pg.Records) < pageSize || seen >= pg.TotalRecords || seen >= maxRecords {
			return 0, false, nil
		}
	}
}

// RemoveAndBlocklist issues DELETE /api/v3/queue/{id} with the flag set
// that makes Sonarr (a) drop the entry from its queue, (b) tell the
// download client to remove the torrent and its files, (c) add the release
// to the series-specific blocklist so it won't be re-grabbed, and (d)
// immediately re-search for a replacement.
func (c *Client) RemoveAndBlocklist(ctx context.Context, queueID int) error {
	if c == nil {
		return nil
	}
	path := fmt.Sprintf("/api/v3/queue/%d?removeFromClient=true&blocklist=true&skipRedownload=false", queueID)
	return c.do(ctx, "DELETE", path, nil)
}

// do issues a request with the API key and discards the body on success.
func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.APIKey)
	t0 := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		slog.Error("%s %s failed: %v", method, path, err)
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	slog.Debug("%s %s -> %d (%d bytes, %s)", method, path, resp.StatusCode, len(raw), time.Since(t0))
	if resp.StatusCode >= 400 {
		slog.Warn("%s %s -> %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
		return fmt.Errorf("sonarr %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode sonarr response: %w (body=%s)", err, string(raw))
		}
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, "GET", path, out)
}
