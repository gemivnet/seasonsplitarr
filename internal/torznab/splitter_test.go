package torznab

import "testing"

func TestDetect(t *testing.T) {
	cases := []struct {
		in            string
		wantStart     int
		wantEnd       int
		shouldDetect  bool
	}{
		{"Some.Show.S01-S05.COMPLETE.1080p.x264-GRP", 1, 5, true},
		{"Some Show Seasons 1-7 1080p WEB-DL", 1, 7, true},
		{"Some.Show.Series.1-3.PDTV.x264", 1, 3, true},
		{"Some.Show.S03.COMPLETE.1080p.x264-GRP", 0, 0, false},
		{"Some.Show.S03E04.1080p.x264-GRP", 0, 0, false},
		{"Some.Show.S01.S05.Bundle", 1, 5, true},
	}
	for _, tc := range cases {
		got := Detect(tc.in)
		if (got != nil) != tc.shouldDetect {
			t.Errorf("Detect(%q) = %v, want shouldDetect=%v", tc.in, got, tc.shouldDetect)
			continue
		}
		if got != nil && (got.Start != tc.wantStart || got.End != tc.wantEnd) {
			t.Errorf("Detect(%q) = %+v, want Start=%d End=%d", tc.in, got, tc.wantStart, tc.wantEnd)
		}
	}
}

func TestSyntheticTitle(t *testing.T) {
	sr := Detect("Some.Show.S01-S05.COMPLETE.1080p.x264-GRP")
	if sr == nil {
		t.Fatal("expected detection")
	}
	got := SyntheticTitle("Some.Show.S01-S05.COMPLETE.1080p.x264-GRP", sr, 3)
	want := "Some.Show.S03.COMPLETE.1080p.x264-GRP"
	if got != want {
		t.Errorf("SyntheticTitle = %q, want %q", got, want)
	}
}
