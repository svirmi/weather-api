package ops

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Server exposes /healthz and /metrics. It is deliberately independent of
// PostgreSQL: a short DB outage must not restart the collector.
type Server struct {
	srv     *http.Server
	health  atomic.Bool
	log     *slog.Logger
	metrics *Registry
}

func NewServer(addr string, metrics *Registry, log *slog.Logger) *Server {
	s := &Server{metrics: metrics, log: log}
	s.health.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.health.Load() {
			http.Error(w, "stopping", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/metrics", metrics.Handler())
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	return s
}

// Start binds synchronously so startup errors surface immediately.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return err
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("ops server failed", "err", err)
		}
	}()
	s.log.Info("ops server listening", "addr", s.srv.Addr)
	return nil
}

func (s *Server) SetHealthy(v bool) { s.health.Store(v) }

func (s *Server) Metrics() *Registry { return s.metrics }

func (s *Server) Shutdown(ctx context.Context) error {
	s.SetHealthy(false)
	return s.srv.Shutdown(ctx)
}
