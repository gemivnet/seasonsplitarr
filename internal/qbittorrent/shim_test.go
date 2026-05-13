package qbittorrent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gemivnet/seasonsplitarr/internal/store"
)

const (
	testUser = "sonarr"
	testPass = "correct-horse-battery-staple"
	// 40 char hex
	testHash = "1111111111111111111111111111111111111111"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	shim := NewShim(t.TempDir(), testUser, testPass, st)
	srv := httptest.NewServer(shim.Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func login(t *testing.T, srv *httptest.Server, user, pass string) (*http.Response, []*http.Cookie) {
	t.Helper()
	form := url.Values{}
	form.Set("username", user)
	form.Set("password", pass)
	req, _ := http.NewRequest("POST", srv.URL+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, resp.Cookies()
}

func TestLoginRejectsBadCreds(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, creds := range [][2]string{
		{"wrong", testPass},
		{testUser, "wrong"},
		{"", ""},
	} {
		resp, cookies := login(t, srv, creds[0], creds[1])
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "Fails." {
			t.Errorf("creds %v: body=%q, want %q", creds, body, "Fails.")
		}
		for _, c := range cookies {
			if c.Name == "SID" && c.Value != "" {
				t.Errorf("creds %v: SID cookie set on failed login", creds)
			}
		}
	}
}

func TestLoginAcceptsGoodCreds(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, cookies := login(t, srv, testUser, testPass)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "Ok." {
		t.Errorf("body=%q, want %q", body, "Ok.")
	}
	var sid string
	for _, c := range cookies {
		if c.Name == "SID" {
			sid = c.Value
		}
	}
	if len(sid) < 16 {
		t.Errorf("SID cookie missing or too short: %q", sid)
	}
}

// TestProtectedEndpointsRejectWithoutCookie is the high-stakes test: every
// non-auth endpoint must 403 without an SID. This is the regression test
// for the class of bug that broke Huntarr (auth bypass on sensitive routes).
func TestProtectedEndpointsRejectWithoutCookie(t *testing.T) {
	srv, _ := newTestServer(t)
	endpoints := []struct {
		method, path string
	}{
		{"GET", "/api/v2/app/version"},
		{"GET", "/api/v2/app/webapiVersion"},
		{"GET", "/api/v2/app/preferences"},
		{"POST", "/api/v2/torrents/add"},
		{"GET", "/api/v2/torrents/info"},
		{"GET", "/api/v2/torrents/properties"},
		{"GET", "/api/v2/torrents/files"},
		{"POST", "/api/v2/torrents/delete"},
		{"POST", "/api/v2/torrents/setCategory"},
		{"POST", "/api/v2/torrents/createCategory"},
		{"GET", "/api/v2/torrents/categories"},
	}
	for _, ep := range endpoints {
		req, _ := http.NewRequest(ep.method, srv.URL+ep.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", ep.method, ep.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: status=%d, want 403", ep.method, ep.path, resp.StatusCode)
		}
	}
}

func TestProtectedEndpointsRejectBogusCookie(t *testing.T) {
	srv, _ := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/v2/torrents/info", nil)
	req.AddCookie(&http.Cookie{Name: "SID", Value: "not-a-real-token"})
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("bogus cookie: status=%d, want 403", resp.StatusCode)
	}
}

func TestProtectedEndpointsAcceptValidCookie(t *testing.T) {
	srv, _ := newTestServer(t)
	_, cookies := login(t, srv, testUser, testPass)

	req, _ := http.NewRequest("GET", srv.URL+"/api/v2/app/version", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), "v") {
		t.Errorf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestAddMagnetRejectsBadInfohash(t *testing.T) {
	srv, _ := newTestServer(t)
	_, cookies := login(t, srv, testUser, testPass)

	// Path-traversal-shaped infohash + missing-hash + wrong-length cases.
	bad := []string{
		"magnet:?xt=urn:btih:../../etc/passwd",
		"magnet:?xt=urn:btih:tooshort",
		"magnet:?dn=no-hash",
		"http://example.com/foo.torrent",
	}
	for _, m := range bad {
		form := url.Values{}
		form.Set("urls", m)
		req, _ := http.NewRequest("POST", srv.URL+"/api/v2/torrents/add", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("magnet=%q: status=%d, want 400", m, resp.StatusCode)
		}
	}
}

func TestAddMagnetRegistersGrab(t *testing.T) {
	srv, st := newTestServer(t)
	_, cookies := login(t, srv, testUser, testPass)

	magnet := "magnet:?xt=urn:btih:" + testHash + "&dn=Show.S03.x264"
	form := url.Values{}
	form.Set("urls", magnet)
	form.Set("category", "tv-sonarr")
	req, _ := http.NewRequest("POST", srv.URL+"/api/v2/torrents/add", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add: status=%d", resp.StatusCode)
	}

	g, ok := st.Get(testHash)
	if !ok {
		t.Fatal("grab not registered")
	}
	if g.Category != "tv-sonarr" {
		t.Errorf("category=%q", g.Category)
	}
}

func TestInfoReturnsRegisteredGrabs(t *testing.T) {
	srv, st := newTestServer(t)
	_, cookies := login(t, srv, testUser, testPass)

	// Pre-populate with two grabs in different categories.
	st.Put(&store.Grab{SynthHash: testHash, RealHash: testHash, Title: "A", Category: "tv-sonarr", State: store.StateQueued})
	st.Put(&store.Grab{SynthHash: "2222222222222222222222222222222222222222", RealHash: "2222222222222222222222222222222222222222", Title: "B", Category: "other", State: store.StateQueued})

	req, _ := http.NewRequest("GET", srv.URL+"/api/v2/torrents/info?category=tv-sonarr", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var arr []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("got %d entries, want 1", len(arr))
	}
	if arr[0]["hash"] != testHash || arr[0]["category"] != "tv-sonarr" {
		t.Errorf("unexpected entry: %+v", arr[0])
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	srv, _ := newTestServer(t)
	_, cookies := login(t, srv, testUser, testPass)

	// Logout.
	req, _ := http.NewRequest("POST", srv.URL+"/api/v2/auth/logout", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Same cookie should now 403.
	req2, _ := http.NewRequest("GET", srv.URL+"/api/v2/app/version", nil)
	for _, c := range cookies {
		req2.AddCookie(c)
	}
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("post-logout: status=%d, want 403", resp2.StatusCode)
	}
}
