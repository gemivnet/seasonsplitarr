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
		// "S01-03" form (no second S, dash separator). The case from the wild:
		// Anthony Bourdain No Reservations S01-03.
		{"Anthony Bourdain No Reservations S01-03", 1, 3, true},
		{"Some.Show.S01-07.1080p.WEB-DL", 1, 7, true},
		// En-dash variant.
		{"Some Show S01–05 1080p", 1, 5, true},
		// "to" / "thru" / "through" connectors.
		{"Some Show S01 to S07 1080p", 1, 7, true},
		{"Some Show S01 thru S07 1080p", 1, 7, true},
		{"Some Show S01 through S07 1080p", 1, 7, true},
		{"Some Show Seasons 1 to 7", 1, 7, true},
		{"Some Show Series 1 through 5", 1, 5, true},
		// Episode-pair notations must NOT be misread as season ranges.
		{"Some.Show.S07E01-E10.1080p", 0, 0, false},
		{"Some.Show.S07.E01-E03.1080p", 0, 0, false},
		// "S0107" with no separator must not be misread as S01-S07.
		{"Some.Show.S0107.1080p", 0, 0, false},
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

func TestSyntheticInfohashDeterministic(t *testing.T) {
	a := SyntheticInfohash("aabbccddeeff", 3)
	b := SyntheticInfohash("aabbccddeeff", 3)
	c := SyntheticInfohash("aabbccddeeff", 4)
	if a != b {
		t.Errorf("SyntheticInfohash not deterministic: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("SyntheticInfohash collided across seasons: %q", a)
	}
	if len(a) != 40 {
		t.Errorf("SyntheticInfohash length = %d, want 40 (btih)", len(a))
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
