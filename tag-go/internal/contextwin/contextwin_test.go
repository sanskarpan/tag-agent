package contextwin

import "testing"

func TestPct(t *testing.T) {
	cases := []struct {
		used, max int
		want      float64
	}{
		{0, 0, 0.0},
		{0, 128000, 0.0},
		{64000, 128000, 50.0},
		{1, 3, 33.33}, // rounds to 2 decimals like Python round(pct, 2)
		{128000, 128000, 100.0},
		{100, 0, 0.0}, // non-positive window guard
	}
	for _, c := range cases {
		if got := Pct(c.used, c.max); got != c.want {
			t.Errorf("Pct(%d,%d) = %v, want %v", c.used, c.max, got, c.want)
		}
	}
}

func TestBarColor(t *testing.T) {
	cases := []struct {
		used, max int
		want      string
	}{
		{0, 128000, "green"},
		{49999, 100000, "green"},
		{50000, 100000, "yellow"},
		{80000, 100000, "yellow"},
		{80001, 100000, "red"},
		{100, 0, "green"}, // zero window -> 0% -> green
	}
	for _, c := range cases {
		if got := BarColor(c.used, c.max); got != c.want {
			t.Errorf("BarColor(%d,%d) = %q, want %q", c.used, c.max, got, c.want)
		}
	}
}

func TestDefaultMaxTokens(t *testing.T) {
	if DefaultMaxTokens != 128000 {
		t.Errorf("DefaultMaxTokens = %d, want 128000", DefaultMaxTokens)
	}
}
