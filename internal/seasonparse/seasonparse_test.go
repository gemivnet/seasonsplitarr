package seasonparse

import "testing"

func TestFromFilename(t *testing.T) {
	cases := []struct {
		path string
		want int
	}{
		{"Show.S03E04.1080p.x264.mkv", 3},
		{"Show.s03e04.1080p.x264.mkv", 3},
		{"Show.3x04.1080p.mkv", 3},
		{"Show.03x04.1080p.mkv", 3},
		{"Show/Season 3/Show.S03E04.mkv", 3},
		{"Show/Season.03/Show.E04.mkv", 3},
		{"Show/Series 2/Episode 4.mkv", 2},
		{"Show.S00E01.Special.mkv", 0}, // season 0 = specials; we return 0
		{"random.mkv", 0},
		{"Show.2020.1080p.mkv", 0}, // year, not season
	}
	for _, tc := range cases {
		got := FromFilename(tc.path)
		if got != tc.want {
			t.Errorf("FromFilename(%q) = %d, want %d", tc.path, got, tc.want)
		}
	}
}
