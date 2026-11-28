package ops

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
