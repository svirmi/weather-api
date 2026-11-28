package rawproxy

import (
	"net/url"
	"strconv"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)

func TestParseQueryDefaults(t *testing.T) {
	q, err := ParseQuery(url.Values{}, fixedNow, false)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if q.To != fixedNow.Unix() || q.From != fixedNow.Unix()-86400 || q.Limit != DefaultRows || q.Order != "desc" {
		t.Errorf("q = %+v", q)
	}
}

func TestParseQueryBounds(t *testing.T) {
	v := url.Values{}
	v.Set("symbol", "XPTUSDT")
	v.Set("from", "1000")
	v.Set("to", strconv.FormatInt(4600, 10))
	v.Set("limit", "5000")
	v.Set("order", "asc")
	q, err := ParseQuery(v, fixedNow, true)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if q.Symbol != "XPTUSDT" || q.From != 1000 || q.To != 4600 || q.Limit != 5000 || q.Order != "asc" {
		t.Errorf("q = %+v", q)
	}
}

func TestParseQueryRejectsBadInput(t *testing.T) {
	cases := []url.Values{
		{"symbol": {"NOPE"}},
		{}, // missing symbol
		{"symbol": {"XPTUSDT"}, "from": {"10000"}, "to": {"1000"}},
		{"symbol": {"XPTUSDT"}, "from": {"0"}, "to": {"90000"}}, // >24h
		{"symbol": {"XPTUSDT"}, "limit": {"5001"}},
		{"symbol": {"XPTUSDT"}, "limit": {"0"}},
		{"symbol": {"XPTUSDT"}, "order": {"sideways"}},
		{"symbol": {"XPTUSDT"}, "to": {"abc"}},
	}
	for i, v := range cases {
		if _, err := ParseQuery(v, fixedNow, true); err == nil {
			t.Errorf("case %d accepted %v", i, v)
		}
	}
}
