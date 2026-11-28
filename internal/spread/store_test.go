package spread

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeScanner lets the row decoders be tested without a database.
type fakeScanner struct {
	vals []any
}

func (f fakeScanner) Scan(dest ...any) error {
	if len(dest) != len(f.vals) {
		return fmt.Errorf("got %d destinations, want %d", len(dest), len(f.vals))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			v, ok := f.vals[i].(int64)
			if !ok {
				return fmt.Errorf("dest %d: want int64, got %T", i, f.vals[i])
			}
			*p = v
		case *float64:
			v, ok := f.vals[i].(float64)
			if !ok {
				return fmt.Errorf("dest %d: want float64, got %T", i, f.vals[i])
			}
			*p = v
		default:
			return fmt.Errorf("unexpected destination type %T", d)
		}
	}
	return nil
}

func TestScanRows(t *testing.T) {
	p, err := ScanHistoryRow(fakeScanner{vals: []any{
		int64(1764340800), int64(3), 85.1, 85.0, 85.2, 85.15, 4201.0, 49.0,
	}})
	if err != nil {
		t.Fatalf("ScanHistoryRow: %v", err)
	}
	if p.T != 1764340800 || p.N != 3 || p.Ratio != 85.1 || p.AskA != 4201 || p.AskB != 49 {
		t.Errorf("unexpected history point: %+v", p)
	}

	lp, err := ScanLiveRow(fakeScanner{vals: []any{int64(100), 4201.5, 49.25, 85.25}})
	if err != nil {
		t.Fatalf("ScanLiveRow: %v", err)
	}
	if lp.T != 100 || lp.AskA != 4201.5 || lp.AskB != 49.25 || lp.Ratio != 85.25 {
		t.Errorf("unexpected live point: %+v", lp)
	}

	latest, err := ScanLatestRow(fakeScanner{vals: []any{int64(101), 4202.0, 49.3, 85.3}})
	if err != nil {
		t.Fatalf("ScanLatestRow: %v", err)
	}
	if latest.T != 101 || latest.Ratio != 85.3 {
		t.Errorf("unexpected latest: %+v", latest)
	}
}

func TestSQLGuards(t *testing.T) {
	for _, want := range []string{
		"JOIN book_l1 b USING (ts_sec)",
		"a.ts_sec >= $3 AND a.ts_sec < $4",
		"b.ts_sec >= $3 AND b.ts_sec < $4",
		"date_bin($5::interval, ts_sec, to_timestamp(0))",
		"avg(r)::double precision",
	} {
		if !strings.Contains(historySQL, want) {
			t.Errorf("historySQL missing %q", want)
		}
	}
	if !strings.Contains(liveSQL, "LIMIT 500") || !strings.Contains(liveSQL, "b.ts_sec > to_timestamp($3)") {
		t.Errorf("liveSQL missing live guards")
	}
	if !strings.Contains(latestSQL, "now() - interval '1 hour'") || !strings.Contains(latestSQL, "ORDER BY a.ts_sec DESC") {
		t.Errorf("latestSQL missing bound or ordering")
	}
	for _, sql := range []string{extremesCountSQL, extremesMinSQL, extremesMaxSQL} {
		for _, want := range []string{
			"a.symbol = $1 AND b.symbol = $2",
			"a.bid > 0 AND a.ask > 0 AND b.bid > 0 AND b.ask > 0",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("extremes query missing %q", want)
			}
		}
	}
	if !strings.Contains(extremesMinSQL, "ORDER BY r ASC, ts_sec ASC") ||
		!strings.Contains(extremesMaxSQL, "ORDER BY r DESC, ts_sec ASC") {
		t.Error("extremes ordering is not allow-listed")
	}
}

func TestArgs(t *testing.T) {
	pair, _ := ParsePair("XAU-XAG")
	from := time.Unix(100, 0).UTC()
	to := time.Unix(200, 0).UTC()
	ha := HistoryArgs(pair, from, to, "1 minute")
	if len(ha) != 5 || ha[0] != "XAUUSDT" || ha[1] != "XAGUSDT" || ha[4] != "1 minute" {
		t.Errorf("HistoryArgs = %#v", ha)
	}
	la := LiveArgs(pair, 123)
	if len(la) != 3 || la[2] != float64(123) {
		t.Errorf("LiveArgs = %#v", la)
	}
	na := LatestArgs(pair)
	if len(na) != 2 || na[0] != "XAUUSDT" {
		t.Errorf("LatestArgs = %#v", na)
	}
}
