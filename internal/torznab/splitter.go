package torznab

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// SeasonRange describes a contiguous season range detected in a release title.
type SeasonRange struct {
	Start int
	End   int
	// MatchedToken is the literal substring that should be replaced when
	// synthesising per-season titles (e.g. "S01-S05", "Seasons 1-5").
	MatchedToken string
}

// patterns matches the most common ways scene/p2p releases describe a
// multi-season pack. Ordered most-specific first.
var patterns = []*regexp.Regexp{
	// S01-S05, S01.S05, S01_S05
	regexp.MustCompile(`(?i)\bS(\d{1,2})[-._ ]?S(\d{1,2})\b`),
	// Seasons 1-5, Season 1-5
	regexp.MustCompile(`(?i)\bSeasons?[\s._-]*(\d{1,2})[\s._-]*[-–][\s._-]*(\d{1,2})\b`),
	// Series 1-5 (UK)
	regexp.MustCompile(`(?i)\bSeries[\s._-]*(\d{1,2})[\s._-]*[-–][\s._-]*(\d{1,2})\b`),
}

// completePattern matches "Complete Series" / "Complete Collection" — these
// don't carry a season range in the title, so the caller needs another signal
// (file list, NFO, total size) to know how many seasons are in the pack.
// We expose it so the caller can decide whether to skip or probe.
var completePattern = regexp.MustCompile(`(?i)\b(Complete\s+(Series|Collection)|Full\s+Series)\b`)

// Detect returns the season range encoded in a release title, or nil if the
// title is not recognised as a multi-season pack.
func Detect(title string) *SeasonRange {
	for _, re := range patterns {
		if m := re.FindStringSubmatch(title); m != nil {
			start, _ := strconv.Atoi(m[1])
			end, _ := strconv.Atoi(m[2])
			if end <= start || end-start > 30 {
				continue
			}
			return &SeasonRange{Start: start, End: end, MatchedToken: m[0]}
		}
	}
	return nil
}

// IsCompletePack reports whether a title looks like a "Complete Series" pack
// without an explicit numeric range. We don't fan these out yet — that
// requires probing the file list, which is a follow-up.
func IsCompletePack(title string) bool {
	return completePattern.MatchString(title)
}

// SyntheticTitle returns a single-season title derived from a multi-season
// title by replacing the matched range token with S{NN}.
func SyntheticTitle(original string, sr *SeasonRange, season int) string {
	replacement := fmt.Sprintf("S%02d", season)
	return strings.Replace(original, sr.MatchedToken, replacement, 1)
}

// SyntheticGUID returns a deterministic GUID for a (infohash, season) pair so
// the same synthetic release re-resolves to the same identity on re-search.
func SyntheticGUID(infohash string, season int) string {
	h := sha1.Sum([]byte(strings.ToLower(infohash) + ":s" + strconv.Itoa(season)))
	return "seasonsplitarr-" + hex.EncodeToString(h[:])
}
