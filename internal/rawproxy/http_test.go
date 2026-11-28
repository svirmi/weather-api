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
}

func (f *fakeStore) Rows(_ context.Context, _ Query) ([]Row, error)             { return f.rows, nil }
func (f *fakeStore) SpreadRows(_ context.Context, _ Query) ([]SpreadRow, error) { return f.spread, nil }
func (f *fakeStore) Sessions(_ context.Context, _ Query) ([]SessionRow, error) {
	return f.sessions, nil
}

func newTestHandler(store StoreReader) (*Handler, *http.ServeMux) {
	h := NewHandler(store, func() time.Time { return fixedNow }, NewMetrics())
	mux := http.NewServeMux()
	h.Register(mux)
	return h, mux
}

func secured(mux *http.ServeMux) http.Handler {
	return NewCORS("http://127.0.0.1:8080").Middleware(NewAuth("secret").Middleware(mux))
}

func request(h http.Handler, method, target, token, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
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

func TestAuthAndHealth(t *testing.T) {
	_, mux := newTestHandler(&fakeStore{})
	h := secured(mux)

	rec := request(h, http.MethodGet, "/api/v1/rows?symbol=XPTUSDT", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
	assertKeys(t, object(t, rec), "error")

	rec = request(h, http.MethodGet, "/api/v1/rows?symbol=XPTUSDT", "secret", "")
	if rec.Code != http.StatusOK {
		t.Errorf("bearer: %d %s", rec.Code, rec.Body.String())
	}
	rec = request(h, http.MethodGet, "/api/v1/rows?symbol=XPTUSDT", "", "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/rows?symbol=XPTUSDT", nil)
	req.Header.Set("X-API-Key", "secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("X-API-Key: %d", rec.Code)
	}

	rec = request(h, http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("health: %d %q", rec.Code, rec.Body.String())
	}
}

func TestCORS(t *testing.T) {
	_, mux := newTestHandler(&fakeStore{})
	h := secured(mux)

	rec := request(h, http.MethodGet, "/api/v1/rows?symbol=XPTUSDT", "secret", "http://127.0.0.1:8080")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:8080" {
		t.Errorf("allowed origin header = %q", got)
	}
	rec = request(h, http.MethodOptions, "/api/v1/rows", "", "http://127.0.0.1:8080")
	if rec.Code != http.StatusNoContent {
		t.Errorf("allowed preflight = %d", rec.Code)
	}
	rec = request(h, http.MethodOptions, "/api/v1/rows", "", "https://evil.example")
	if rec.Code != http.StatusForbidden {
		t.Errorf("disallowed preflight = %d", rec.Code)
	}
	rec = request(h, http.MethodGet, "/api/v1/rows?symbol=XPTUSDT", "secret", "https://evil.example")
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("disallowed origin got CORS header")
	}
}

func TestRowContracts(t *testing.T) {
	store := &fakeStore{
		rows:     []Row{{TS: 1000, ExchTime: 999, Bid: 1, BidQty: 2, Ask: 3, AskQty: 4}},
		spread:   []SpreadRow{{TS: 1000, Symbol: "XPTUSDT", Bid: 1, Ask: 3, BidQty: 2, AskQty: 4, Spread: 2, Mid: 2}},
		sessions: []SessionRow{{Start: 1000, End: 4600, Kind: "REGULAR"}},
	}
	_, mux := newTestHandler(store)

	rec := request(secured(mux), http.MethodGet, "/api/v1/rows?symbol=XPTUSDT", "secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rows: %d %s", rec.Code, rec.Body.String())
	}
	obj := object(t, rec)
	assertKeys(t, obj, "symbol", "from", "to", "limit", "rows")
	assertKeys(t, obj["rows"].([]any)[0].(map[string]any), "ts", "exchTime", "bid", "bidQty", "ask", "askQty")

	rec = request(secured(mux), http.MethodGet, "/api/v1/spread?symbol=XPTUSDT", "secret", "")
	obj = object(t, rec)
	assertKeys(t, obj["rows"].([]any)[0].(map[string]any), "ts", "symbol", "bid", "ask", "bidQty", "askQty", "spread", "mid")

	rec = request(secured(mux), http.MethodGet, "/api/v1/sessions", "secret", "")
	obj = object(t, rec)
	assertKeys(t, obj, "from", "to", "limit", "rows")
	assertKeys(t, obj["rows"].([]any)[0].(map[string]any), "start", "end", "kind")
}

func TestEmptyRowsAreArrays(t *testing.T) {
	_, mux := newTestHandler(&fakeStore{})
	rec := request(secured(mux), http.MethodGet, "/api/v1/rows?symbol=XAUUSDT", "secret", "")
	if !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestValidation(t *testing.T) {
	_, mux := newTestHandler(&fakeStore{})
	h := secured(mux)
	for _, target := range []string{
		"/api/v1/rows?symbol=NOPE",
		"/api/v1/rows",
		"/api/v1/rows?symbol=XPTUSDT&limit=5001",
		"/api/v1/spread?symbol=XPTUSDT&from=0&to=90000",
		"/api/v1/sessions?order=sideways",
	} {
		rec := request(h, http.MethodGet, target, "secret", "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", target, rec.Code)
		}
		assertKeys(t, object(t, rec), "error")
	}
}

func TestMetrics(t *testing.T) {
	_, mux := newTestHandler(&fakeStore{})
	h := secured(mux)
	request(h, http.MethodGet, "/api/v1/rows?symbol=XPTUSDT", "secret", "")
	rec := request(h, http.MethodGet, "/metrics", "secret", "")
	body := rec.Body.String()
	if !strings.Contains(body, `rawproxy_requests_total{path="/api/v1/rows"} 1`) ||
		!strings.Contains(body, "rawproxy_errors_total 0") {
		t.Errorf("metrics = %s", body)
	}
}
