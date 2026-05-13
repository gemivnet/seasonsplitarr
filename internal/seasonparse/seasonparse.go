// Package seasonparse extracts a season number from an episode filename.
// Used by the grabber to group an RD torrent's files into per-season buckets.
package seasonparse

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var patterns = []*regexp.Regexp{
	// S01E02, s1e2, S01.E02
	regexp.MustCompile(`(?i)\bS(\d{1,2})[\s._-]?E\d{1,3}\b`),
	// 1x02, 01x02
	regexp.MustCompile(`(?i)\b(\d{1,2})x\d{1,3}\b`),
	// Season 1, Season.01
	regexp.MustCompile(`(?i)\bSeason[\s._-]*(\d{1,2})\b`),
	// Series 1 (UK)
	regexp.MustCompile(`(?i)\bSeries[\s._-]*(\d{1,2})\b`),
	// "S01" anywhere (last resort — matches more, may overmatch)
	regexp.MustCompile(`(?i)\bS(\d{1,2})\b`),
}

// FromFilename returns the season number embedded in a file path, or 0 if
// none can be confidently inferred. The path is checked against both the
// basename and the parent directory (RD packs commonly use Season folders).
func FromFilename(path string) int {
	base := filepath.Base(path)
	parent := filepath.Base(filepath.Dir(path))
	candidates := []string{base, parent, path}
	for _, re := range patterns {
		for _, c := range candidates {
			if m := re.FindStringSubmatch(c); m != nil {
				n, _ := strconv.Atoi(strings.TrimLeft(m[1], "0"))
				if n > 0 && n < 100 {
					return n
				}
			}
		}
	}
	return 0
}
