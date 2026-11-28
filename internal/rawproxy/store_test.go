package rawproxy

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

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
		case *string:
			v, ok := f.vals[i].(string)
			if !ok {
				return fmt.Errorf("dest %d: want string, got %T", i, f.vals[i])
			}
			*p = v
		default:
			return fmt.Errorf("unexpected destination type %T", d)
		}
	}
	return nil
}

func TestSQLGuards(t *testing.T) {
	for _, sql := range []string{rowsDescSQL, rowsAscSQL, spreadDescSQL, spreadAscSQL} {
		for _, want := range []string{
			"symbol = $1",
			"ts_sec >= to_timestamp($2) AND ts_sec < to_timestamp($3)",
			"LIMIT $4",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("query missing %q", want)
			}
		}
	}
	if !strings.Contains(rowsDescSQL, "ORDER BY ts_sec DESC") || !strings.Contains(rowsAscSQL, "ORDER BY ts_sec ASC") {
		t.Error("rows ordering is not allow-listed")
	}
	if !strings.Contains(spreadDescSQL, "FROM book_l1_spread") {
		t.Error("spread query does not use the view")
	}
	for _, sql := range []string{sessionsDescSQL, sessionsAscSQL} {
		if !strings.Contains(sql, "FROM session_window") || !strings.Contains(sql, "LIMIT $3") {
			t.Error("session query missing table or bound")
		}
	}
}

func TestArgsAndRowDecoding(t *testing.T) {
	q := Query{Symbol: "XAUUSDT", From: 1000, To: 4600, Limit: 5000, Order: "desc"}
	ra := RowsArgs(q)
	if len(ra) != 4 || ra[0] != "XAUUSDT" || ra[1] != float64(1000) || ra[3] != 5000 {
		t.Errorf("RowsArgs = %#v", ra)
	}
	sa := SessionsArgs(q)
	if len(sa) != 3 || sa[1] != float64(4600) {
		t.Errorf("SessionsArgs = %#v", sa)
	}

	row, err := ScanRow(fakeScanner{vals: []any{int64(1000), int64(999), 4200.1, 1.5, 4200.3, 2.5}})
	if err != nil || row.TS != 1000 || row.ExchTime != 999 || row.Ask != 4200.3 || row.AskQty != 2.5 {
		t.Errorf("ScanRow = %+v, %v", row, err)
	}
	srow, err := ScanSpreadRow(fakeScanner{vals: []any{
		int64(1000), "XAUUSDT", 4200.1, 4200.3, 1.5, 2.5, 0.2, 4200.2,
	}})
	if err != nil || srow.Symbol != "XAUUSDT" || srow.Spread != 0.2 || srow.Mid != 4200.2 {
		t.Errorf("ScanSpreadRow = %+v, %v", srow, err)
	}
	sess, err := ScanSessionRow(fakeScanner{vals: []any{int64(1000), int64(4600), "REGULAR"}})
	if err != nil || sess.Kind != "REGULAR" || sess.End != 4600 {
		t.Errorf("ScanSessionRow = %+v, %v", sess, err)
	}
	if !q.WindowStart().Equal(time.Unix(1000, 0).UTC()) || !q.WindowEnd().Equal(time.Unix(4600, 0).UTC()) {
		t.Error("window bounds are wrong")
	}
}
