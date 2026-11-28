package spread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	historyPath = "/api/v1/spread"
	livePath    = "/api/v1/spread/live"
	cacheTTL    = 60 * time.Second
	maxCacheKey = 10
)

// StoreReader is the Read side used by the handlers. A fixture-backed fake in
// the contract tests implements it, so tests never need a database.
type StoreReader interface {
	History(ctx context.Context, pair Pair, preset Preset, from, to time.Time) ([]Point, error)
	Live(ctx context.Context, pair Pair, after int64) ([]LivePoint, error)
	Latest(ctx context.Context, pair Pair) (*Latest, error)
}

// Clock is injected so cache and timestamp behaviour can be tested without
// sleeping.
type Clock func() time.Time

type cacheKey struct {
	Pair  string
	Range string
}

type cacheEntry struct {
	body    []byte
	expires time.Time
}

// Cache stores final history JSON bodies for a bounded set of pair/range keys.
type Cache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     Clock
	entries map[cacheKey]cacheEntry
}

// NewCache creates a history cache with an injectable clock.
func NewCache(ttl time.Duration, now Clock) *Cache {
	return &Cache{ttl: ttl, now: now, entries: make(map[cacheKey]cacheEntry)}
}

// Get returns a cached body if it has not expired.
func (c *Cache) Get(key cacheKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.expires) {
		delete(c.entries, key)
		return nil, false
	}
	return e.body, true
}

// Put stores a body, evicting the earliest-expiring entry when full.
func (c *Cache) Put(key cacheKey, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxCacheKey {
		if _, ok := c.entries[key]; !ok {
			var oldest cacheKey
			var earliest time.Time
			first := true
			for k, e := range c.entries {
				if first || e.expires.Before(earliest) {
					oldest, earliest, first = k, e.expires, false
				}
			}
			delete(c.entries, oldest)
		}
	}
	c.entries[key] = cacheEntry{body: body, expires: c.now().Add(c.ttl)}
}

// Metrics is a small local metrics registry. It deliberately does not import
// the collector's ops package or bind its 9090 port.
type Metrics struct {
	mu          sync.Mutex
	requests    map[string]int64
	errors      int64
	cacheHits   int64
	lastQueryMS float64
}

// NewMetrics creates an empty registry.
func NewMetrics() *Metrics { return &Metrics{requests: make(map[string]int64)} }

func (m *Metrics) record(path string, failed, cacheHit bool, elapsed time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[path]++
	if failed {
		m.errors++
	}
	if cacheHit {
		m.cacheHits++
	}
	m.lastQueryMS = float64(elapsed) / float64(time.Millisecond)
}

// Render returns the metrics in plain text.
func (m *Metrics) Render() string {
	m.mu.Lock()
	paths := make([]string, 0, len(m.requests))
	for p := range m.requests {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	counts := make([]int64, len(paths))
	for i, p := range paths {
		counts[i] = m.requests[p]
	}
	errs, hits, last := m.errors, m.cacheHits, m.lastQueryMS
	m.mu.Unlock()

	var b strings.Builder
	fmt.Fprintln(&b, "# TYPE spread_requests_total counter")
	for i, p := range paths {
		fmt.Fprintf(&b, "spread_requests_total{path=%q} %d\n", p, counts[i])
	}
	fmt.Fprintln(&b, "# TYPE spread_errors_total counter")
	fmt.Fprintf(&b, "spread_errors_total %d\n", errs)
	fmt.Fprintln(&b, "# TYPE spread_cache_hits_total counter")
	fmt.Fprintf(&b, "spread_cache_hits_total %d\n", hits)
	fmt.Fprintln(&b, "# TYPE spread_last_query_ms gauge")
	fmt.Fprintf(&b, "spread_last_query_ms %g\n", last)
	return b.String()
}

// Handler serves the metrics in plain text on the API's own port.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, m.Render())
	})
}

// Handler owns the v1 HTTP surface.
type Handler struct {
	Store   StoreReader
	Cache   *Cache
	Now     Clock
	PollMS  int
	Metrics *Metrics
}

// NewHandler wires the handlers with injectable dependencies.
func NewHandler(store StoreReader, cache *Cache, now Clock, pollMS int, metrics *Metrics) *Handler {
	return &Handler{Store: store, Cache: cache, Now: now, PollMS: pollMS, Metrics: metrics}
}

// RegisterAPI attaches only the pair-series routes. Use this when mounting
// the API inside another service that already has /healthz and /metrics.
func (h *Handler) RegisterAPI(mux *http.ServeMux) {
	mux.HandleFunc(historyPath, h.handleHistory)
	mux.HandleFunc(livePath, h.handleLive)
}

// Register attaches the v1 routes to a mux. CORS is applied by the caller.
func (h *Handler) Register(mux *http.ServeMux) {
	h.RegisterAPI(mux)
	mux.HandleFunc("/healthz", h.handleHealth)
	mux.Handle("/metrics", h.Metrics.Handler())
}

// CORS allows the local page to call the API from file:// or a static server.
// The wildcard is for the local phase only; a deployed service must narrow it.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	pair, err := ParsePair(r.URL.Query().Get("pair"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	preset, err := ParsePreset(r.URL.Query().Get("range"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	to := h.Now().UTC()
	from := to.Add(-preset.Window)
	key := cacheKey{Pair: pair.ID, Range: preset.Name}
	if body, ok := h.Cache.Get(key); ok {
		h.Metrics.record(historyPath, false, true, 0)
		writeBody(w, body)
		return
	}

	start := h.Now()
	points, err := h.Store.History(r.Context(), pair, preset, from, to)
	elapsed := h.Now().Sub(start)
	if err != nil {
		h.Metrics.record(historyPath, true, false, elapsed)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if points == nil {
		points = []Point{}
	}
	body, err := json.Marshal(HistoryResponse{
		Pair:      pair.ID,
		Range:     preset.Name,
		BucketSec: preset.BucketSec(),
		AsOf:      to.Unix(),
		Points:    points,
	})
	if err != nil {
		h.Metrics.record(historyPath, true, false, elapsed)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	h.Cache.Put(key, body)
	h.Metrics.record(historyPath, false, false, elapsed)
	writeBody(w, body)
}

func (h *Handler) handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	pair, err := ParsePair(r.URL.Query().Get("pair"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	var after int64
	if raw := r.URL.Query().Get("after"); raw != "" {
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid after: %s", raw))
			return
		}
	} else {
		after = h.Now().UTC().Unix() - 300
	}

	start := h.Now()
	points, err := h.Store.Live(r.Context(), pair, after)
	if err != nil {
		h.Metrics.record(livePath, true, false, h.Now().Sub(start))
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	latest, err := h.Store.Latest(r.Context(), pair)
	if err != nil {
		h.Metrics.record(livePath, true, false, h.Now().Sub(start))
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	elapsed := h.Now().Sub(start)
	if points == nil {
		points = []LivePoint{}
	}
	cursor := after
	if len(points) > 0 {
		cursor = points[len(points)-1].T
	}
	writeJSON(w, http.StatusOK, LiveResponse{
		Pair:   pair.ID,
		Cursor: cursor,
		PollMS: h.PollMS,
		Latest: latest,
		Points: points,
	})
	h.Metrics.record(livePath, false, false, elapsed)
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
	writeBodyWithStatus(w, status, body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, ErrorResponse{Error: err.Error()})
}

func writeBody(w http.ResponseWriter, body []byte) {
	writeBodyWithStatus(w, http.StatusOK, body)
}

func writeBodyWithStatus(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
