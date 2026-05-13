package torznab

import (
	"crypto/subtle"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Proxy is a Torznab proxy that fans out multi-season pack results into
// per-season synthetic releases.
type Proxy struct {
	UpstreamURL    string
	UpstreamAPIKey string
	// LocalAPIKey is the key that incoming requests (from Prowlarr/Sonarr)
	// must present.
	LocalAPIKey string
	Client      *http.Client
	// OnSynthetic is called for every synthetic per-season release emitted.
	// Implementations typically register the grab in the state store so the
	// download client side can resolve synthHash -> (realHash, magnet, season).
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
		http.Error(w, "server misconfigured: no apikey set", http.StatusInternalServerError)
		return
	}
	got := r.URL.Query().Get("apikey")
	if subtle.ConstantTimeCompare([]byte(got), []byte(p.LocalAPIKey)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	upstream, err := url.Parse(p.UpstreamURL)
	if err != nil {
		http.Error(w, "bad upstream config", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	q.Set("apikey", p.UpstreamAPIKey)
	upstream.RawQuery = q.Encode()

	resp, err := p.client().Get(upstream.String())
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "upstream read error", http.StatusBadGateway)
		return
	}

	// Only attempt to rewrite XML search responses. Caps, errors, etc. pass through.
	mode := strings.ToLower(q.Get("t"))
	if mode != "search" && mode != "tvsearch" && mode != "movie" {
		passThrough(w, resp, body)
		return
	}

	rewritten, err := splitFeed(body, p.OnSynthetic)
	if err != nil {
		// On any parse error, fail open: serve the upstream response unchanged.
		passThrough(w, resp, body)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(rewritten)
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
	w.Write(body)
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

func splitFeed(body []byte, onSynth func(synthHash, realHash, magnet string, season int, title string)) ([]byte, error) {
	var feed rss
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, err
	}
	var out []item
	for _, it := range feed.Channel.Items {
		sr := Detect(it.Title)
		if sr == nil {
			out = append(out, it)
			continue
		}
		ih := extractInfohash(it)
		magnet := magnetFromItem(it)
		for s := sr.Start; s <= sr.End; s++ {
			si := synthItem(it, sr, s, ih)
			if onSynth != nil && ih != "" {
				synthHash := SyntheticInfohash(ih, s)
				onSynth(synthHash, strings.ToLower(ih), magnet, s, si.Title)
			}
			out = append(out, si)
		}
	}
	feed.Channel.Items = out
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
