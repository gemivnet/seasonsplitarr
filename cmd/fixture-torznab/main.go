// fixture-torznab is a tiny standalone Torznab server that serves a canned
// RSS feed. It lets us exercise seasonsplitarr's Torznab proxy + qBit shim
// without pointing at a real Prowlarr or hitting any indexer. Used in the
// local smoke-test phase of the internal test plan.
//
// Usage:
//
//	go run ./cmd/fixture-torznab -addr :9999 -apikey fixture-key
//
// Then point seasonsplitarr at it:
//
//	SS_UPSTREAM_URL=http://localhost:9999/api
//	SS_UPSTREAM_APIKEY=fixture-key
package main

import (
	"crypto/subtle"
	"flag"
	"fmt"
	"log"
	"net/http"
)

const feed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <title>fixture-torznab</title>
    <description>Canned multi-season pack for testing seasonsplitarr</description>
    <link>http://localhost:9999/</link>
    <item>
      <title>My.Show.S01-S03.COMPLETE.1080p.x264-FIXTURE</title>
      <guid isPermaLink="false">fixture-pack-1</guid>
      <pubDate>Mon, 13 May 2026 00:00:00 +0000</pubDate>
      <size>53687091200</size>
      <link>magnet:?xt=urn:btih:0000000000000000000000000000000000000001&amp;dn=My.Show.S01-S03.COMPLETE.1080p.x264-FIXTURE</link>
      <enclosure url="magnet:?xt=urn:btih:0000000000000000000000000000000000000001&amp;dn=My.Show.S01-S03.COMPLETE.1080p.x264-FIXTURE" length="53687091200" type="application/x-bittorrent" />
      <torznab:attr name="infohash" value="0000000000000000000000000000000000000001" />
      <torznab:attr name="seeders" value="50" />
      <torznab:attr name="peers" value="60" />
    </item>
    <item>
      <title>Another.Show.Seasons.1-7.1080p.WEB-DL-FIXTURE</title>
      <guid isPermaLink="false">fixture-pack-2</guid>
      <pubDate>Mon, 13 May 2026 00:00:00 +0000</pubDate>
      <size>214748364800</size>
      <link>magnet:?xt=urn:btih:0000000000000000000000000000000000000002&amp;dn=Another.Show.Seasons.1-7</link>
      <enclosure url="magnet:?xt=urn:btih:0000000000000000000000000000000000000002&amp;dn=Another.Show.Seasons.1-7" length="214748364800" type="application/x-bittorrent" />
      <torznab:attr name="infohash" value="0000000000000000000000000000000000000002" />
    </item>
    <item>
      <title>Single.Season.Show.S04.1080p.x264-FIXTURE</title>
      <guid isPermaLink="false">fixture-single</guid>
      <pubDate>Mon, 13 May 2026 00:00:00 +0000</pubDate>
      <size>16106127360</size>
      <link>magnet:?xt=urn:btih:0000000000000000000000000000000000000003&amp;dn=Single.Season.Show.S04</link>
      <enclosure url="magnet:?xt=urn:btih:0000000000000000000000000000000000000003&amp;dn=Single.Season.Show.S04" length="16106127360" type="application/x-bittorrent" />
      <torznab:attr name="infohash" value="0000000000000000000000000000000000000003" />
    </item>
  </channel>
</rss>`

const caps = `<?xml version="1.0" encoding="UTF-8"?>
<caps>
  <server title="fixture-torznab"/>
  <limits max="100" default="100"/>
  <searching>
    <search available="yes" supportedParams="q"/>
    <tv-search available="yes" supportedParams="q,season,ep"/>
  </searching>
  <categories>
    <category id="5000" name="TV"/>
  </categories>
</caps>`

func main() {
	addr := flag.String("addr", ":9999", "listen address")
	apikey := flag.String("apikey", "fixture-key", "API key required by clients")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("apikey")
		if subtle.ConstantTimeCompare([]byte(got), []byte(*apikey)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Query().Get("t") {
		case "caps":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(caps))
		default:
			w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
			_, _ = w.Write([]byte(feed))
		}
	})

	log.Printf("fixture-torznab listening on %s (apikey=%s)", *addr, *apikey)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * 1_000_000_000}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// Compile-time use to keep fmt available for future debug prints.
var _ = fmt.Sprint
