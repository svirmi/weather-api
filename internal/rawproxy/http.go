package rawproxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
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
	Symbol string `json:"symbol"`
	From   int64  `json:"from"`
	To     int64  `json:"to"`
	Limit  int    `json:"limit"`
	Rows   []Row  `json:"rows"`
}

type SpreadResponse struct {
	Symbol string      `json:"symbol"`
	From   int64       `json:"from"`
	To     int64       `json:"to"`
	Limit  int         `json:"limit"`
	Rows   []SpreadRow `json:"rows"`
}

type SessionsResponse struct {
	From  int64        `json:"from"`
	To    int64        `json:"to"`
	Limit int          `json:"limit"`
	Rows  []SessionRow `json:"rows"`
}

// StoreReader is the database side used by handlers; the contract tests use a
// fixture-backed fake.
type StoreReader interface {
	Rows(ctx context.Context, q Query) ([]Row, error)
	SpreadRows(ctx context.Context, q Query) ([]SpreadRow, error)
	Sessions(ctx context.Context, q Query) ([]SessionRow, error)
}

// Auth enforces a bearer token on every path except /healthz.
type Auth struct {
	token []byte
}

// NewAuth creates the token gate.
func NewAuth(token string) *Auth { return &Auth{token: []byte(token)} }

// Middleware wraps a handler with token authentication.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		raw := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(raw, "Bearer ") {
			raw = strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
		} else if key := r.Header.Get("X-API-Key"); key != "" {
			raw = key
		}
		if raw == "" || subtle.ConstantTimeCompare([]byte(raw), a.token) != 1 {
			writeJSON(w, http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CORS allows only explicitly configured origins.
type CORS struct {
	allowed map[string]bool
}

// NewCORS builds an origin allow-list from a comma-separated string.
func NewCORS(list string) *CORS {
	m := make(map[string]bool)
	for _, origin := range strings.Split(list, ",") {
		if o := strings.TrimSpace(origin); o != "" {
			m[o] = true
		}
	}
	return &CORS{allowed: m}
}

// Middleware sets CORS headers only for allowed origins.
func (c *CORS) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && c.allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-API-Key, Content-Type")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		} else if r.Method == http.MethodOptions {
			writeJSON(w, http.StatusForbidden, ErrorResponse{Error: "origin not allowed"})
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

// Handler serves the counters in plain text.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintln(w, "# TYPE rawproxy_requests_total counter")
		for i, p := range paths {
			fmt.Fprintf(w, "rawproxy_requests_total{path=%q} %d\n", p, counts[i])
		}
		fmt.Fprintln(w, "# TYPE rawproxy_errors_total counter")
		fmt.Fprintf(w, "rawproxy_errors_total %d\n", errs)
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

// Register attaches routes. Auth and CORS are applied by the caller.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/rows", h.handleRows)
	mux.HandleFunc("/api/v1/spread", h.handleSpread)
	mux.HandleFunc("/api/v1/sessions", h.handleSessions)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/metrics", h.Metrics.Handler())
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
	writeJSON(w, http.StatusOK, RowsResponse{Symbol: q.Symbol, From: q.From, To: q.To, Limit: q.Limit, Rows: rows})
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
	writeJSON(w, http.StatusOK, SpreadResponse{Symbol: q.Symbol, From: q.From, To: q.To, Limit: q.Limit, Rows: rows})
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
