package rawproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type ErrorResponse struct {
	Error string `json:"error"`
}

type RowsResponse struct {
	Symbol     string `json:"symbol"`
	From       int64  `json:"from"`
	To         int64  `json:"to"`
	Limit      int    `json:"limit"`
	NextCursor int64  `json:"next_cursor,omitempty"`
	Rows       []Row  `json:"rows"`
}

type SpreadResponse struct {
	Symbol     string      `json:"symbol"`
	From       int64       `json:"from"`
	To         int64       `json:"to"`
	Limit      int         `json:"limit"`
	NextCursor int64       `json:"next_cursor,omitempty"`
	Rows       []SpreadRow `json:"rows"`
}

type SessionsResponse struct {
	From  int64        `json:"from"`
	To    int64        `json:"to"`
	Limit int          `json:"limit"`
	Rows  []SessionRow `json:"rows"`
}

type StatsResponse struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
	Stats
}

// StoreReader is the database side used by handlers; the contract tests use a
// fixture-backed fake.
type StoreReader interface {
	Rows(ctx context.Context, q Query) ([]Row, error)
	SpreadRows(ctx context.Context, q Query) ([]SpreadRow, error)
	Sessions(ctx context.Context, q Query) ([]SessionRow, error)
	Stats(ctx context.Context, q Query) (Stats, error)
}

// DefaultMiddleware makes the proxy easy to use from scripts and browsers.
// The service is intentionally unauthenticated; it only ever issues read-only
// SQL and lives behind the platform's public HTTPS endpoint.
func DefaultMiddleware(next http.Handler) http.Handler {
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

// RateLimiter is a simple in-process token bucket. It is a mitigation for the
// public data port, not a substitute for platform authentication: every client
// behind the ingress shares the bucket.
type RateLimiter struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	rate   float64
	burst  float64
}

func NewRateLimiter(rate, burst float64) *RateLimiter {
	if rate <= 0 || burst <= 0 {
		panic("rate and burst must be positive")
	}
	return &RateLimiter{tokens: burst, last: time.Now(), rate: rate, burst: burst}
}

func (rl *RateLimiter) allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	rl.tokens += now.Sub(rl.last).Seconds() * rl.rate
	if rl.tokens > rl.burst {
		rl.tokens = rl.burst
	}
	rl.last = now
	if rl.tokens < 1 {
		return false
	}
	rl.tokens--
	return true
}

// Middleware rejects excess requests with 429 before they reach the DB pool.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Metrics is a small local registry for the proxy.
type Metrics struct {
	mu       sync.Mutex
	requests map[string]int64
	errors   int64
}

// NewMetrics creates an empty registry.
func NewMetrics() *Metrics { return &Metrics{requests: make(map[string]int64)} }

func (m *Metrics) record(path string, failed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[path]++
	if failed {
		m.errors++
	}
}

// Render returns the counters in plain text.
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
	errs := m.errors
	m.mu.Unlock()

	var b strings.Builder
	fmt.Fprintln(&b, "# TYPE rawproxy_requests_total counter")
	for i, p := range paths {
		fmt.Fprintf(&b, "rawproxy_requests_total{path=%q} %d\n", p, counts[i])
	}
	fmt.Fprintln(&b, "# TYPE rawproxy_errors_total counter")
	fmt.Fprintf(&b, "rawproxy_errors_total %d\n", errs)
	return b.String()
}

// Handler serves the counters in plain text.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, m.Render())
	})
}

// Handler owns the proxy's HTTP surface.
type Handler struct {
	Store   StoreReader
	Now     func() time.Time
	Metrics *Metrics
}

// NewHandler wires the handlers.
func NewHandler(store StoreReader, now func() time.Time, metrics *Metrics) *Handler {
	return &Handler{Store: store, Now: now, Metrics: metrics}
}

// RegisterAPI attaches only the data endpoints. Use this when mounting the
// proxy inside another service that already has /healthz and /metrics.
func (h *Handler) RegisterAPI(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/rows", h.handleRows)
	mux.HandleFunc("/api/v1/sessions", h.handleSessions)
	mux.HandleFunc("/api/v1/stats", h.handleStats)
	mux.Handle("/api/v1/metrics", h.Metrics.Handler())
}

// Register attaches all routes, including standalone health and metrics.
// Callers wrap the mux with DefaultMiddleware.
func (h *Handler) Register(mux *http.ServeMux) {
	h.RegisterAPI(mux)
	mux.HandleFunc("/api/v1/spread", h.handleSpread)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/metrics", h.Metrics.Handler())
}

func nextCursor(ts []int64, order string, limit int) int64 {
	if order != "asc" || len(ts) < limit || len(ts) == 0 {
		return 0
	}
	return ts[len(ts)-1] + 1
}

func rowTimes(rows []Row) []int64 {
	out := make([]int64, len(rows))
	for i := range rows {
		out[i] = rows[i].TS
	}
	return out
}

func spreadTimes(rows []SpreadRow) []int64 {
	out := make([]int64, len(rows))
	for i := range rows {
		out[i] = rows[i].TS
	}
	return out
}

func (h *Handler) handleRows(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}
	q, err := ParseQuery(r.URL.Query(), h.Now(), true)
	if err != nil {
		h.Metrics.record("/api/v1/rows", true)
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	rows, err := h.Store.Rows(r.Context(), q)
	if err != nil {
		h.Metrics.record("/api/v1/rows", true)
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	if rows == nil {
		rows = []Row{}
	}
	h.Metrics.record("/api/v1/rows", false)
	writeJSON(w, http.StatusOK, RowsResponse{
		Symbol: q.Symbol, From: q.From, To: q.To, Limit: q.Limit,
		NextCursor: nextCursor(rowTimes(rows), q.Order, q.Limit), Rows: rows,
	})
}

func (h *Handler) handleSpread(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}
	q, err := ParseQuery(r.URL.Query(), h.Now(), true)
	if err != nil {
		h.Metrics.record("/api/v1/spread", true)
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	rows, err := h.Store.SpreadRows(r.Context(), q)
	if err != nil {
		h.Metrics.record("/api/v1/spread", true)
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	if rows == nil {
		rows = []SpreadRow{}
	}
	h.Metrics.record("/api/v1/spread", false)
	writeJSON(w, http.StatusOK, SpreadResponse{
		Symbol: q.Symbol, From: q.From, To: q.To, Limit: q.Limit,
		NextCursor: nextCursor(spreadTimes(rows), q.Order, q.Limit), Rows: rows,
	})
}

func (h *Handler) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}
	q, err := ParseQuery(r.URL.Query(), h.Now(), false)
	if err != nil {
		h.Metrics.record("/api/v1/sessions", true)
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	rows, err := h.Store.Sessions(r.Context(), q)
	if err != nil {
		h.Metrics.record("/api/v1/sessions", true)
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	if rows == nil {
		rows = []SessionRow{}
	}
	h.Metrics.record("/api/v1/sessions", false)
	writeJSON(w, http.StatusOK, SessionsResponse{From: q.From, To: q.To, Limit: q.Limit, Rows: rows})
}

func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}
	q, err := ParseQuery(r.URL.Query(), h.Now(), false)
	if err != nil {
		h.Metrics.record("/api/v1/stats", true)
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	stats, err := h.Store.Stats(r.Context(), q)
	if err != nil {
		h.Metrics.record("/api/v1/stats", true)
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	h.Metrics.record("/api/v1/stats", false)
	writeJSON(w, http.StatusOK, StatsResponse{From: q.From, To: q.To, Stats: stats})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
