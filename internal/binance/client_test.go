package binance

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 5 * time.Second},
		{"7", 7 * time.Second},
		{"0", 5 * time.Second},
		{"garbage", 5 * time.Second},
		{"9999", 5 * time.Minute},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
	httpDate := now.Add(9 * time.Second).Format(http.TimeFormat)
	if got := parseRetryAfter(httpDate, now); got != 9*time.Second {
		t.Errorf("HTTP-date retry-after = %s, want 9s", got)
	}
}

func TestKindOf(t *testing.T) {
	err := &APIError{Kind: KindBan, Err: errors.New("banned")}
	if got := KindOf(err); got != KindBan {
		t.Fatalf("KindOf = %s, want ban", got)
	}
	if got := KindOf(context.DeadlineExceeded); got != KindTimeout {
		t.Fatalf("KindOf deadline = %s, want timeout", got)
	}
	if got := KindOf(errors.New("x")); got != KindUnknown {
		t.Fatalf("KindOf plain = %s, want unknown", got)
	}
}
