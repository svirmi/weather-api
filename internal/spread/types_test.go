package spread

import (
	"testing"
	"time"
)

func TestPairsAreFrozen(t *testing.T) {
	if len(Pairs) != 2 {
		t.Fatalf("len(Pairs) = %d, want 2", len(Pairs))
	}
	for _, tc := range []struct{ id, num, den string }{
		{"XAU-XAG", "XAUUSDT", "XAGUSDT"},
		{"XPT-XPD", "XPTUSDT", "XPDUSDT"},
	} {
		p, err := ParsePair(tc.id)
		if err != nil {
			t.Fatalf("ParsePair(%q): %v", tc.id, err)
		}
		if p.Num != tc.num || p.Den != tc.den {
			t.Errorf("%s = %s/%s, want %s/%s", tc.id, p.Num, p.Den, tc.num, tc.den)
		}
	}
	if _, err := ParsePair("XAG-XAU"); err == nil {
		t.Error("ParsePair accepted an unknown pair")
	}
}

func TestPresetsAreFrozen(t *testing.T) {
	want := []struct {
		name string
		win  time.Duration
		sec  int64
	}{
		{"45d", 45 * 24 * time.Hour, 900},
		{"7d", 7 * 24 * time.Hour, 300},
		{"1d", 24 * time.Hour, 60},
		{"8h", 8 * time.Hour, 15},
		{"1h", time.Hour, 1},
	}
	if len(Presets) != len(want) {
		t.Fatalf("len(Presets) = %d, want %d", len(Presets), len(want))
	}
	for i, tc := range want {
		p, err := ParsePreset(tc.name)
		if err != nil {
			t.Fatalf("ParsePreset(%q): %v", tc.name, err)
		}
		if Presets[i].Name != tc.name || p.Window != tc.win || p.BucketSec() != tc.sec {
			t.Errorf("preset %d = %+v, want %s window=%s sec=%d", i, p, tc.name, tc.win, tc.sec)
		}
	}
	if _, err := ParsePreset("30d"); err == nil {
		t.Error("ParsePreset accepted an unknown range")
	}
}
