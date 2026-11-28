package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunAggregates(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/stats":
			_, _ = w.Write([]byte(`{
				"from":1000,"to":2000,
				"symbols":[{"symbol":"XAUUSDT","rows":100,"first_ts":1000,"last_ts":1100,
					"max_gap_sec":2,"duplicates":0,"bid_lt_ask_ratio":1}],
				"partitions":[],"future_partitions":7,"future_ok":true,"aged_partitions":[]
			}`))
		case "/api/v1/rows":
			pages++
			if r.URL.Query().Get("cursor") == "" {
				_, _ = w.Write([]byte(`{"from":1000,"to":2000,"limit":5000,"next_cursor":1002,
					"rows":[{"ts":1000,"bid":99.5,"ask":100.5},
					        {"ts":1001,"bid":99,"ask":101}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"from":1000,"to":2000,"limit":5000,
				"rows":[{"ts":1002,"bid":98.5,"ask":101.5}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	report, err := run(context.Background(), srv.Client(), srv.URL, 1000, 2000, "XAUUSDT")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if pages != 2 {
		t.Errorf("spread pages = %d, want 2", pages)
	}
	if len(report.Window.Symbols) != 1 || report.Window.Symbols[0].CoveragePct != 100 {
		t.Errorf("symbols = %+v", report.Window.Symbols)
	}
	s := report.Spread
	if s.Rows != 3 || s.MeanSpread != 2 || s.Median != 2 || s.P95 != 3 || s.Min != 1 || s.Max != 3 || s.MeanBPS != 200 {
		t.Errorf("spread = %+v", s)
	}
}

func TestRunRejectsBadWindow(t *testing.T) {
	if _, err := run(context.Background(), http.DefaultClient, "http://unused", 2000, 1000, "XAUUSDT"); err == nil {
		t.Error("accepted inverted window")
	}
}
