package spread

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
	history []Point
	live    []LivePoint
	latest  *Latest
	points  int64
	min     *Point
	max     *Point

	histErr, liveErr, latestErr, extremesErr error
	historyCalls, liveCalls                  int
	latestCalls, extremesCalls               int
}

func (f *fakeStore) History(_ context.Context, _ Pair, _ Preset, _, _ time.Time) ([]Point, error) {
	f.historyCalls++
	return f.history, f.histErr
}

func (f *fakeStore) Live(_ context.Context, _ Pair, _ int64) ([]LivePoint, error) {
	f.liveCalls++
	return f.live, f.liveErr
}

func (f *fakeStore) Latest(_ context.Context, _ Pair) (*Latest, error) {
	f.latestCalls++
	return f.latest, f.latestErr
}

func (f *fakeStore) Extremes(_ context.Context, _ Pair, _ Preset, _, _ time.Time) (int64, *Point, *Point, error) {
	f.extremesCalls++
	return f.points, f.min, f.max, f.extremesErr
}

func newTestHandler(store StoreReader, now *time.Time) (*Handler, *http.ServeMux) {
	cache := NewCache(time.Minute, func() time.Time { return *now })
	h := NewHandler(store, cache, func() time.Time { return *now }, 1000, NewMetrics())
	mux := http.NewServeMux()
	h.Register(mux)
	return h, mux
}

func request(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
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

func TestHistoryContract(t *testing.T) {
	now := time.Date(2026, 11, 28, 8, 0, 0, 0, time.UTC)
	store := &fakeStore{history: []Point{{
		T: 1764340800, N: 3, Ratio: 85.123456,
		Lo: 85.0, Hi: 85.2, Last: 85.15, AskA: 4201.35, AskB: 49.36,
	}}}
	_, mux := newTestHandler(store, &now)

	rec := request(t, mux, http.MethodGet, "/api/v1/spread?pair=XAU-XAG&range=1h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	obj := object(t, rec)
	assertKeys(t, obj, "pair", "range", "bucketSec", "asOf", "points")
	if obj["pair"] != "XAU-XAG" || obj["range"] != "1h" {
		t.Errorf("pair/range = %v/%v", obj["pair"], obj["range"])
	}
	if obj["bucketSec"] != float64(1) || obj["asOf"] != float64(now.Unix()) {
		t.Errorf("bucketSec/asOf = %v/%v", obj["bucketSec"], obj["asOf"])
	}
	points, ok := obj["points"].([]any)
	if !ok || len(points) != 1 {
		t.Fatalf("points = %#v", obj["points"])
	}
	assertKeys(t, points[0].(map[string]any), "t", "n", "ratio", "lo", "hi", "last", "askA", "askB")
}

func TestExtremesContract(t *testing.T) {
	now := time.Date(2026, 11, 28, 8, 0, 0, 0, time.UTC)
	store := &fakeStore{
		points: 100,
		min:    &Point{T: 1000, N: 1, Ratio: 80, AskA: 4000, AskB: 50},
		max:    &Point{T: 2000, N: 1, Ratio: 90, AskA: 4500, AskB: 50},
	}
	_, mux := newTestHandler(store, &now)

	// A history call for the same pair/range must not collide with extremes.
	request(t, mux, http.MethodGet, "/api/v1/spread?pair=XAU-XAG&range=1d")
	rec := request(t, mux, http.MethodGet, "/api/v1/spread/extremes?pair=XAU-XAG&range=1d")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	obj := object(t, rec)
	assertKeys(t, obj, "pair", "range", "basis", "points", "min", "max", "range_pct")
	if obj["basis"] != "ask" || obj["points"] != float64(100) || obj["range_pct"] != 12.5 {
		t.Errorf("extremes = %#v", obj)
	}
	min := obj["min"].(map[string]any)
	assertKeys(t, min, "ts", "ratio", "price_a", "price_b")
	if min["ts"] != float64(1000) || min["price_a"] != float64(4000) || min["price_b"] != float64(50) {
		t.Errorf("min = %#v", min)
	}

	request(t, mux, http.MethodGet, "/api/v1/spread/extremes?pair=XAU-XAG&range=1d")
	if store.extremesCalls != 1 {
		t.Errorf("extremes calls = %d, want 1 (cached)", store.extremesCalls)
	}
}

func TestExtremesNotEnoughData(t *testing.T) {
	now := time.Date(2026, 11, 28, 8, 0, 0, 0, time.UTC)
	_, mux := newTestHandler(&fakeStore{points: 1}, &now)
	rec := request(t, mux, http.MethodGet, "/api/v1/spread/extremes?pair=XPT-XPD&range=1h")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	assertKeys(t, object(t, rec), "error")
}

func TestEmptyHistoryIsArray(t *testing.T) {
	now := time.Date(2026, 11, 28, 8, 0, 0, 0, time.UTC)
	_, mux := newTestHandler(&fakeStore{}, &now)
	rec := request(t, mux, http.MethodGet, "/api/v1/spread?pair=XPT-XPD&range=45d")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"points":[]`) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestLiveContract(t *testing.T) {
	now := time.Date(2026, 11, 28, 8, 0, 0, 0, time.UTC)
	store := &fakeStore{
		live:   []LivePoint{{T: 100, AskA: 1, AskB: 2, Ratio: 0.5}, {T: 101, AskA: 2, AskB: 2, Ratio: 1}},
		latest: &Latest{T: 101, AskA: 2, AskB: 2, Ratio: 1},
	}
	_, mux := newTestHandler(store, &now)

	rec := request(t, mux, http.MethodGet, "/api/v1/spread/live?pair=XPT-XPD&after=99")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	obj := object(t, rec)
	assertKeys(t, obj, "pair", "cursor", "pollMs", "latest", "points")
	if obj["pair"] != "XPT-XPD" || obj["cursor"] != float64(101) || obj["pollMs"] != float64(1000) {
		t.Errorf("unexpected live header: %#v", obj)
	}
	assertKeys(t, obj["latest"].(map[string]any), "t", "askA", "askB", "ratio")
	points := obj["points"].([]any)
	if len(points) != 2 {
		t.Fatalf("len(points) = %d", len(points))
	}
	assertKeys(t, points[0].(map[string]any), "t", "askA", "askB", "ratio")
}

func TestLiveEmptyCursorAndNullLatest(t *testing.T) {
	now := time.Date(2026, 11, 28, 8, 0, 0, 0, time.UTC)
	_, mux := newTestHandler(&fakeStore{}, &now)
	rec := request(t, mux, http.MethodGet, "/api/v1/spread/live?pair=XAU-XAG&after=50")
	obj := object(t, rec)
	if rec.Code != http.StatusOK || obj["cursor"] != float64(50) {
		t.Fatalf("status = %d, cursor = %v", rec.Code, obj["cursor"])
	}
	if v, ok := obj["latest"]; !ok || v != nil {
		t.Errorf("latest = %#v, want explicit null", v)
	}
	if s := strings.TrimSpace(rec.Body.String()); !strings.Contains(s, `"points":[]`) {
		t.Errorf("body = %s", s)
	}
}

func TestValidationErrors(t *testing.T) {
	now := time.Date(2026, 11, 28, 8, 0, 0, 0, time.UTC)
	_, mux := newTestHandler(&fakeStore{}, &now)
	for _, tc := range []struct {
		name, target string
	}{
		{"pair", "/api/v1/spread?pair=NOPE&range=1h"},
		{"range", "/api/v1/spread?pair=XAU-XAG&range=30d"},
		{"after", "/api/v1/spread/live?pair=XAU-XAG&after=abc"},
	} {
		rec := request(t, mux, http.MethodGet, tc.target)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d", tc.name, rec.Code)
		}
		assertKeys(t, object(t, rec), "error")
	}
}

func TestCacheAndMetrics(t *testing.T) {
	now := time.Date(2026, 11, 28, 8, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	h, mux := newTestHandler(store, &now)
	target := "/api/v1/spread?pair=XAU-XAG&range=1h"

	request(t, mux, http.MethodGet, target)
	request(t, mux, http.MethodGet, target)
	if store.historyCalls != 1 {
		t.Errorf("historyCalls = %d, want 1 (cache hit)", store.historyCalls)
	}

	now = now.Add(61 * time.Second)
	request(t, mux, http.MethodGet, target)
	if store.historyCalls != 2 {
		t.Errorf("historyCalls = %d, want 2 after expiry", store.historyCalls)
	}

	rec := request(t, mux, http.MethodGet, "/metrics")
	body := rec.Body.String()
	if !strings.Contains(body, `spread_requests_total{path="/api/v1/spread"} 3`) ||
		!strings.Contains(body, "spread_cache_hits_total 1") {
		t.Errorf("metrics body = %s", body)
	}
	_ = h
}

func TestCORS(t *testing.T) {
	now := time.Date(2026, 11, 28, 8, 0, 0, 0, time.UTC)
	_, mux := newTestHandler(&fakeStore{}, &now)
	wrapped := CORS(mux)

	rec := request(t, wrapped, http.MethodOptions, "/api/v1/spread")
	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q", got)
	}
	rec = request(t, wrapped, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("health = %d %q", rec.Code, rec.Body.String())
	}
}
