package torznab

import (
	"crypto/subtle"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gemivnet/seasonsplitarr/internal/logging"
)

var plog = logging.New("torznab")

// Upstream is one Torznab indexer the proxy fans out to.
type Upstream struct {
	URL    string
	APIKey string
}

// Proxy is a Torznab proxy that fans out multi-season pack results into
// per-season synthetic releases. When configured with multiple Upstreams,
// search queries are executed against all of them in parallel and the
// merged result is split.
type Proxy struct {
	// Upstreams is the list of Torznab indexers to query.
	Upstreams []Upstream
	// LocalAPIKey is the key that incoming requests (from Prowlarr/Sonarr)
	// must present.
	LocalAPIKey string
	Client      *http.Client
	// OnSynthetic is called for every synthetic per-season release emitted.
	OnSynthetic func(synthHash, realHash, magnet string, season int, title string)
}

func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api", p.handleAPI)
	mux.HandleFunc("/", p.handleAPI) // tolerate /api or root
	return mux
}

func (p *Proxy) handleAPI(w http.ResponseWriter, r *http.Request) {
	if p.LocalAPIKey == "" {
		plog.Error("torznab request rejected: LocalAPIKey not configured")
		http.Error(w, "server misconfigured: no apikey set", http.StatusInternalServerError)
		return
	}
	got := r.URL.Query().Get("apikey")
	if subtle.ConstantTimeCompare([]byte(got), []byte(p.LocalAPIKey)) != 1 {
		plog.Warn("torznab unauthorized: apikey mismatch (got=%s len=%d, expected len=%d) from %s",
			logging.Redact(got), len(got), len(p.LocalAPIKey), r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	q := r.URL.Query()
	mode := strings.ToLower(q.Get("t"))
	plog.Info("torznab query: t=%s q=%q season=%s ep=%s cat=%s upstreams=%d",
		mode, q.Get("q"), q.Get("season"), q.Get("ep"), q.Get("cat"), len(p.Upstreams))

	if len(p.Upstreams) == 0 {
		plog.Error("no upstreams configured")
		http.Error(w, "no upstreams configured", http.StatusInternalServerError)
		return
	}

	// Caps and other non-search queries: only the first upstream answers.
	// They're identical-shaped across indexers and Prowlarr already aggregates
	// them for clients.
	isSearch := mode == "search" || mode == "tvsearch" || mode == "movie"
	if !isSearch {
		resp, body, ok := p.fetchOne(p.Upstreams[0], q)
		if !ok {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		plog.Debug("passthrough (mode=%q) from %s", mode, p.Upstreams[0].URL)
		passThrough(w, resp, body)
		return
	}

	// Fan out search across all upstreams in parallel.
	type result struct {
		idx  int
		body []byte
		ct   string
	}
	results := make(chan result, len(p.Upstreams))
	start := time.Now()
	for i, up := range p.Upstreams {
		go func(i int, up Upstream) {
			resp, body, ok := p.fetchOne(up, q)
			if !ok {
				results <- result{idx: i}
				return
			}
			defer resp.Body.Close()
			results <- result{idx: i, body: body, ct: resp.Header.Get("Content-Type")}
		}(i, up)
	}

	bodies := make([][]byte, len(p.Upstreams))
	for range p.Upstreams {
		r := <-results
		bodies[r.idx] = r.body
	}
	plog.Info("fan-out complete: %d upstreams queried in %s", len(p.Upstreams), time.Since(start))

	rewritten, err := splitFeedMulti(bodies, p.OnSynthetic)
	if err != nil {
		plog.Warn("splitFeedMulti failed (%v) — falling back to first non-empty upstream", err)
		for _, b := range bodies {
			if len(b) > 0 {
				w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(b)
				return
			}
		}
		http.Error(w, "all upstreams failed", http.StatusBadGateway)
		return
	}
	plog.Info("served merged+rewritten feed (%d bytes)", len(rewritten))
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rewritten)
}

// fetchOne queries a single upstream and returns the response. Caller must
// close resp.Body. Returns ok=false on any error (logged here).
func (p *Proxy) fetchOne(up Upstream, q url.Values) (*http.Response, []byte, bool) {
	parsed, err := url.Parse(up.URL)
	if err != nil {
		plog.Error("bad upstream URL %q: %v", up.URL, err)
		return nil, nil, false
	}
	uq := url.Values{}
	for k, v := range q {
		uq[k] = v
	}
	uq.Set("apikey", up.APIKey)
	parsed.RawQuery = uq.Encode()
	t0 := time.Now()
	resp, err := p.client().Get(parsed.String())
	if err != nil {
		plog.Error("upstream %s error: %v", up.URL, err)
		return nil, nil, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		plog.Error("upstream %s read error: %v", up.URL, err)
		_ = resp.Body.Close()
		return nil, nil, false
	}
	plog.Info("upstream %s -> %d (%d bytes, %s)", up.URL, resp.StatusCode, len(body), time.Since(t0))
	return resp, body, true
}

func (p *Proxy) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

func passThrough(w http.ResponseWriter, resp *http.Response, body []byte) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// --- feed rewriting ---

// Minimal Torznab/RSS structures. We use a generic any-attribute approach so
// we don't drop upstream-specific extensions.
type rss struct {
	XMLName xml.Name `xml:"rss"`
	Attrs   []xml.Attr `xml:",any,attr"`
	Channel channel  `xml:"channel"`
}

type channel struct {
	Title       string  `xml:"title,omitempty"`
	Description string  `xml:"description,omitempty"`
	Link        string  `xml:"link,omitempty"`
	Items       []item  `xml:"item"`
	Extras      []anyEl `xml:",any"`
}

type item struct {
	Title    string    `xml:"title"`
	GUID     string    `xml:"guid,omitempty"`
	Link     string    `xml:"link,omitempty"`
	PubDate  string    `xml:"pubDate,omitempty"`
	Size     string    `xml:"size,omitempty"`
	Enclosure *enclosure `xml:"enclosure,omitempty"`
	Attrs    []anyEl   `xml:",any"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Length string `xml:"length,attr,omitempty"`
	Type   string `xml:"type,attr,omitempty"`
}

type anyEl struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Inner   string     `xml:",innerxml"`
}

// stripXmlnsAttrs removes any xmlns* declarations from attrs. Go's xml
// encoder auto-emits namespace declarations when serializing inner elements
// that carry a Space (like torznab:attr), so preserving xmlns from the
// upstream causes duplicate-attribute errors in strict parsers (Sonarr).
func stripXmlnsAttrs(attrs []xml.Attr) []xml.Attr {
	out := attrs[:0]
	for _, a := range attrs {
		if a.Name.Local == "xmlns" || a.Name.Space == "xmlns" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// splitItems runs the multi-season detection + per-season fan-out over a
// list of items, optionally notifying onSynth for each synthetic emitted.
func splitItems(items []item, onSynth func(synthHash, realHash, magnet string, season int, title string)) []item {
	out := make([]item, 0, len(items))
	for _, it := range items {
		sr := Detect(it.Title)
		if sr == nil {
			out = append(out, it)
			continue
		}
		ih := extractInfohash(it)
		magnet := magnetFromItem(it)
		plog.Info("  detected pack: %q -> seasons %d..%d (realhash=%s)",
			it.Title, sr.Start, sr.End, shortHash(ih))
		for s := sr.Start; s <= sr.End; s++ {
			si := synthItem(it, sr, s, ih)
			if onSynth != nil && ih != "" {
				synthHash := SyntheticInfohash(ih, s)
				plog.Info("    -> S%02d title=%q synthHash=%s", s, si.Title, shortHash(synthHash))
				onSynth(synthHash, strings.ToLower(ih), magnet, s, si.Title)
			}
			out = append(out, si)
		}
	}
	return out
}

// splitFeedMulti parses multiple upstream feed bodies, merges their items
// (deduping by infohash), and splits multi-season packs. Empty/failed bodies
// are skipped. Returns an error only if every body fails to parse.
func splitFeedMulti(bodies [][]byte, onSynth func(synthHash, realHash, magnet string, season int, title string)) ([]byte, error) {
	var merged rss
	merged.Channel.Title = "seasonsplitarr"
	merged.Channel.Description = "Merged Torznab feed from multiple upstreams"
	seen := map[string]bool{}
	parsedCount := 0
	for i, body := range bodies {
		if len(body) == 0 {
			continue
		}
		var feed rss
		if err := xml.Unmarshal(body, &feed); err != nil {
			plog.Warn("upstream #%d: parse failed (%v), skipping", i, err)
			continue
		}
		parsedCount++
		if merged.Attrs == nil {
			merged.Attrs = stripXmlnsAttrs(feed.Attrs)
		}
		for _, it := range feed.Channel.Items {
			ih := strings.ToLower(extractInfohash(it))
			if ih != "" {
				if seen[ih] {
					continue
				}
				seen[ih] = true
			}
			merged.Channel.Items = append(merged.Channel.Items, it)
		}
	}
	if parsedCount == 0 {
		return nil, fmt.Errorf("no upstream feeds parsed")
	}
	merged.Channel.Items = splitItems(merged.Channel.Items, onSynth)
	plog.Info("merged %d upstream feeds -> %d items after split", parsedCount, len(merged.Channel.Items))
	buf, err := xml.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), buf...), nil
}

func splitFeed(body []byte, onSynth func(synthHash, realHash, magnet string, season int, title string)) ([]byte, error) {
	var feed rss
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, err
	}
	feed.Attrs = stripXmlnsAttrs(feed.Attrs)
	plog.Debug("splitFeed: %d upstream items", len(feed.Channel.Items))
	feed.Channel.Items = splitItems(feed.Channel.Items, onSynth)
	buf, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), buf...), nil
}

func synthItem(orig item, sr *SeasonRange, season int, infohash string) item {
	clone := orig
	clone.Title = SyntheticTitle(orig.Title, sr, season)
	id := infohash
	if id == "" {
		// Fall back to hashing the original GUID; still deterministic across calls.
		id = orig.GUID
	}
	clone.GUID = SyntheticGUID(id, season)
	synthHash := SyntheticInfohash(id, season)

	// Rewrite magnet URLs so the synthetic release carries its own infohash.
	// qBit-shaped download clients (and Sonarr's grab-tracking) key on the
	// btih in the magnet — without this, all synthetic seasons for one pack
	// would collide on the same hash.
	if clone.Enclosure != nil {
		clone.Enclosure.URL = rewriteMagnetInfohash(clone.Enclosure.URL, synthHash)
	}
	if clone.Link != "" {
		clone.Link = rewriteMagnetInfohash(clone.Link, synthHash)
	}
	// Also rewrite a torznab:attr infohash element if present so Prowlarr/Sonarr
	// pick up the synthetic hash from any path they choose.
	for i, a := range clone.Attrs {
		if !strings.EqualFold(a.XMLName.Local, "attr") {
			continue
		}
		var nameVal string
		for _, at := range a.Attrs {
			if strings.EqualFold(at.Name.Local, "name") {
				nameVal = at.Value
			}
		}
		if !strings.EqualFold(nameVal, "infohash") {
			continue
		}
		for j, at := range a.Attrs {
			if strings.EqualFold(at.Name.Local, "value") {
				clone.Attrs[i].Attrs[j].Value = synthHash
			}
		}
	}
	return clone
}

// rewriteMagnetInfohash replaces the xt=urn:btih:<hash> component of a magnet
// URL with the supplied synthetic hash. Non-magnet URLs and inputs that
// don't contain a btih are returned unchanged.
func rewriteMagnetInfohash(s, newHash string) string {
	const xt = "xt=urn:btih:"
	i := strings.Index(s, xt)
	if i < 0 {
		return s
	}
	rest := s[i+len(xt):]
	end := strings.IndexAny(rest, "&")
	if end < 0 {
		return s[:i+len(xt)] + newHash
	}
	return s[:i+len(xt)] + newHash + rest[end:]
}

func extractInfohash(it item) string {
	for _, a := range it.Attrs {
		if strings.EqualFold(a.XMLName.Local, "attr") {
			var name, value string
			for _, at := range a.Attrs {
				switch strings.ToLower(at.Name.Local) {
				case "name":
					name = at.Value
				case "value":
					value = at.Value
				}
			}
			if strings.EqualFold(name, "infohash") {
				return value
			}
		}
	}
	// Try to parse from magnet link in enclosure.
	if it.Enclosure != nil {
		if h := infohashFromMagnet(it.Enclosure.URL); h != "" {
			return h
		}
	}
	if h := infohashFromMagnet(it.Link); h != "" {
		return h
	}
	return ""
}

// magnetFromItem returns the magnet URL on an item, preferring enclosure.url.
func magnetFromItem(it item) string {
	if it.Enclosure != nil && strings.HasPrefix(it.Enclosure.URL, "magnet:") {
		return it.Enclosure.URL
	}
	if strings.HasPrefix(it.Link, "magnet:") {
		return it.Link
	}
	return ""
}

func infohashFromMagnet(s string) string {
	const prefix = "magnet:?xt=urn:btih:"
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	rest := s[i+len(prefix):]
	if amp := strings.IndexAny(rest, "&"); amp >= 0 {
		rest = rest[:amp]
	}
	return rest
}

// Ensure xml.Marshal emits unknown elements when reading anyEl.
var _ = fmt.Sprint

func shortHash(h string) string {
	if len(h) < 10 {
		return h
	}
	return h[:8] + "…"
}
