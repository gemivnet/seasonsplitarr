// Package debrid is a minimal Real-Debrid API client. Only the endpoints
// seasonsplitarr actually needs are implemented.
//
// API reference: https://api.real-debrid.com/
package debrid

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gemivnet/seasonsplitarr/internal/logging"
)

var dlog = logging.New("rd")

const baseURL = "https://api.real-debrid.com/rest/1.0"

type Client struct {
	Token string
	HTTP  *http.Client
}

func New(token string) *Client {
	return &Client{
		Token: token,
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}
}

// AddMagnetResult is the response from /torrents/addMagnet.
type AddMagnetResult struct {
	ID  string `json:"id"`
	URI string `json:"uri"`
}

// File describes one file inside an RD torrent.
type File struct {
	ID       int    `json:"id"`
	Path     string `json:"path"` // includes leading slash and folders
	Bytes    int64  `json:"bytes"`
	Selected int    `json:"selected"` // 0 or 1
}

// TorrentInfo is the response from /torrents/info/{id}.
type TorrentInfo struct {
	ID       string   `json:"id"`
	Filename string   `json:"filename"`
	Hash     string   `json:"hash"`
	Bytes    int64    `json:"bytes"`
	Status   string   `json:"status"` // magnet_conversion, waiting_files_selection, queued, downloading, downloaded, error, ...
	Progress int      `json:"progress"`
	Files    []File   `json:"files"`
	Links    []string `json:"links"` // RD restricted links, one per selected file in path order
}

// UnrestrictResult is the response from /unrestrict/link.
type UnrestrictResult struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	Filesize int64  `json:"filesize"`
	Download string `json:"download"` // direct https URL
}

func (c *Client) AddMagnet(ctx context.Context, magnet string) (*AddMagnetResult, error) {
	form := url.Values{}
	form.Set("magnet", magnet)
	var out AddMagnetResult
	if err := c.do(ctx, "POST", "/torrents/addMagnet", form, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) TorrentInfo(ctx context.Context, id string) (*TorrentInfo, error) {
	var out TorrentInfo
	if err := c.do(ctx, "GET", "/torrents/info/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SelectFiles tells RD which files inside the torrent to download. Pass "all"
// or a list of file IDs.
func (c *Client) SelectFiles(ctx context.Context, id string, fileIDs []int) error {
	form := url.Values{}
	if len(fileIDs) == 0 {
		form.Set("files", "all")
	} else {
		parts := make([]string, len(fileIDs))
		for i, f := range fileIDs {
			parts[i] = strconv.Itoa(f)
		}
		form.Set("files", strings.Join(parts, ","))
	}
	return c.do(ctx, "POST", "/torrents/selectFiles/"+id, form, nil)
}

func (c *Client) Unrestrict(ctx context.Context, link string) (*UnrestrictResult, error) {
	form := url.Values{}
	form.Set("link", link)
	var out UnrestrictResult
	if err := c.do(ctx, "POST", "/unrestrict/link", form, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteTorrent(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/torrents/delete/"+id, nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	t0 := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		dlog.Error("%s %s failed: %v", method, path, err)
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	dlog.Debug("%s %s -> %d (%d bytes, %s)", method, path, resp.StatusCode, len(raw), time.Since(t0))
	if resp.StatusCode >= 400 {
		dlog.Warn("%s %s -> %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
		return fmt.Errorf("real-debrid %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w (body=%s)", err, string(raw))
	}
	return nil
}

// DownloadFile streams the contents of a direct URL to dst (a writer). The
// caller is responsible for opening the destination file. Returns bytes
// written.
func (c *Client) DownloadFile(ctx context.Context, directURL string, dst io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", directURL, nil)
	if err != nil {
		return 0, err
	}
	// Use a dedicated client without the short timeout — downloads can be large.
	dl := &http.Client{Timeout: 6 * time.Hour}
	resp, err := dl.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("download %s: %d %s", directURL, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return io.Copy(dst, resp.Body)
}
