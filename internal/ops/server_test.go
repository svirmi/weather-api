package ops

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRenderWithSource(t *testing.T) {
	r := NewRegistry()
	r.Counter("own_total", nil).Inc()
	r.AddSource("extra", func() string { return "# TYPE extra_total counter\nextra_total 7\n" })
	got := r.Render()
	if !strings.Contains(got, "own_total 1") || !strings.Contains(got, "extra_total 7") {
		t.Errorf("render = %q", got)
	}
}

func TestMount(t *testing.T) {
	s := NewServer("127.0.0.1:0", NewRegistry(), slog.Default())
	s.Mount("/api/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path))
	}))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "/api/v1/stats" {
		t.Errorf("mounted handler: %d %q", rec.Code, rec.Body.String())
	}
}
