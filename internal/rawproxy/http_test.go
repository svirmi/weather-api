package rawproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type fakeStore struct {
	rows     []Row
	spread   []SpreadRow
	sessions []SessionRow
	stats    Stats
}

func (f *fakeStore) Rows(_ context.Context, _ Query) ([]Row, error)             { return f.rows, nil }
func (f *fakeStore) SpreadRows(_ context.Context, _ Query) ([]SpreadRow, error) { return f.spread, nil }
func (f *fakeStore) Sessions(_ context.Context, _ Query) ([]SessionRow, error) {
	return f.sessions, nil
}
func (f *fakeStore) Stats(_ context.Context, _ Query) (Stats, error) { return f.stats, nil }

func newTestHandler(store StoreReader) (*Handler, http.Handler) {
	h := NewHandler(store, func() time.Time { return fixedNow }, NewMetrics())
	mux := http.NewServeMux()
	h.Register(mux)
	return h, DefaultMiddleware(mux)
}

func request(h http.Handler, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func object(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func assertKeys(t *testing.T, m map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

func TestHealthAndOptions(t *testing.T) {
	_, h := newTestHandler(&fakeStore{})
	rec := request(h, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("health: %d %q", rec.Code, rec.Body.String())
	}
	rec = request(h, http.MethodOptions, "/api/v1/rows")
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("preflight: %d origin=%q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestRowContractsAndCursor(t *testing.T) {
	store := &fakeStore{
		rows:     []Row{{TS: 1000, ExchTime: 999, Bid: 1, BidQty: 2, Ask: 3, AskQty: 4}},
		spread:   []SpreadRow{{TS: 1000, Symbol: "XPTUSDT", Bid: 1, Ask: 3, BidQty: 2, AskQty: 4, Spread: 2, Mid: 2}},
		sessions: []SessionRow{{Start: 1000, End: 4600, Kind: "REGULAR"}},
	}
	_, h := newTestHandler(store)

	rec := request(h, http.MethodGet, "/api/v1/rows?symbol=XPTUSDT&order=asc&limit=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("rows: %d %s", rec.Code, rec.Body.String())
	}
	obj := object(t, rec)
	assertKeys(t, obj, "symbol", "from", "to", "limit", "next_cursor", "rows")
	if obj["next_cursor"] != float64(1001) {
		t.Errorf("next_cursor = %v, want 1001", obj["next_cursor"])
	}
	assertKeys(t, obj["rows"].([]any)[0].(map[string]any), "ts", "exchTime", "bid", "bidQty", "ask", "askQty")

	rec = request(h, http.MethodGet, "/api/v1/rows?symbol=XPTUSDT&order=desc&limit=1")
	if _, ok := object(t, rec)["next_cursor"]; ok {
		t.Error("desc response should not carry next_cursor")
	}

	rec = request(h, http.MethodGet, "/api/v1/spread?symbol=XPTUSDT")
	if rec.Code != http.StatusOK {
		t.Fatalf("spread: %d %s", rec.Code, rec.Body.String())
	}
	obj = object(t, rec)
	assertKeys(t, obj["rows"].([]any)[0].(map[string]any), "ts", "symbol", "bid", "ask", "bidQty", "askQty", "spread", "mid")

	rec = request(h, http.MethodGet, "/api/v1/sessions")
	obj = object(t, rec)
	assertKeys(t, obj, "from", "to", "limit", "rows")
	assertKeys(t, obj["rows"].([]any)[0].(map[string]any), "start", "end", "kind")
}

func TestStatsContract(t *testing.T) {
	store := &fakeStore{stats: Stats{
		Symbols:          []SymbolStats{{Symbol: "XPTUSDT", Rows: 10, MaxGapSec: 2, Duplicates: 1, BidLTAskRatio: 0.99}},
		Partitions:       []string{"book_l1_20261128"},
		FuturePartitions: 7,
		FutureOK:         true,
		AgedPartitions:   []string{},
	}}
	_, h := newTestHandler(store)
	rec := request(h, http.MethodGet, "/api/v1/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats: %d %s", rec.Code, rec.Body.String())
	}
	obj := object(t, rec)
	assertKeys(t, obj, "from", "to", "symbols", "partitions", "future_partitions", "future_ok", "aged_partitions")
	sym := obj["symbols"].([]any)[0].(map[string]any)
	assertKeys(t, sym, "symbol", "rows", "first_ts", "last_ts", "max_gap_sec", "duplicates", "bid_lt_ask_ratio")
}

func TestEmptyRowsAreArrays(t *testing.T) {
	_, h := newTestHandler(&fakeStore{})
	rec := request(h, http.MethodGet, "/api/v1/rows?symbol=XAUUSDT")
	if !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestValidation(t *testing.T) {
	_, h := newTestHandler(&fakeStore{})
	for _, target := range []string{
		"/api/v1/rows?symbol=NOPE",
		"/api/v1/rows",
		"/api/v1/rows?symbol=XPTUSDT&limit=5001",
		"/api/v1/rows?symbol=XPTUSDT&order=desc&cursor=1000",
		"/api/v1/spread?symbol=XPTUSDT&from=0&to=90000",
		"/api/v1/sessions?order=sideways",
		"/api/v1/stats?from=1000&to=0",
	} {
		rec := request(h, http.MethodGet, target)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", target, rec.Code)
		}
		assertKeys(t, object(t, rec), "error")
	}
}

func TestRateLimiter(t *testing.T) {
	limiter := NewRateLimiter(1, 1)
	h := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if rec := request(h, http.MethodGet, "/api/v1/stats"); rec.Code != http.StatusOK {
		t.Errorf("first request: %d", rec.Code)
	}
	if rec := request(h, http.MethodGet, "/api/v1/stats"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("second request: %d, want 429", rec.Code)
	}
}

func TestMetrics(t *testing.T) {
	_, h := newTestHandler(&fakeStore{})
	request(h, http.MethodGet, "/api/v1/rows?symbol=XPTUSDT")
	rec := request(h, http.MethodGet, "/metrics")
	body := rec.Body.String()
	if !strings.Contains(body, `rawproxy_requests_total{path="/api/v1/rows"} 1`) ||
		!strings.Contains(body, "rawproxy_errors_total 0") {
		t.Errorf("metrics = %s", body)
	}
}
