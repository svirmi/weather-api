// Command spreadapi serves the read-only v1 pair-spread API for local testing.
// It is deliberately separate from the collector: no migrations, no writes,
// and no shared ops port.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"metals-collector/internal/spread"
)

const (
	defaultAddr = "127.0.0.1:9091"
	historyTTL  = 60 * time.Second
	pollMS      = 1000
)

type config struct {
	DSN  string
	Addr string
}

// loadConfig mirrors only the DSN alias logic of internal/config. It must not
// import that package: its Load() validates collector-only variables and
// defaults METRICS_ADDR to :9090.
func loadConfig() (config, error) {
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return config{}, errors.New("DB_DSN (or DATABASE_URL) is required")
	}
	addr := os.Getenv("SPREAD_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	return config{DSN: dsn, Addr: addr}, nil
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := loadConfig()
	if err != nil {
		log.Error("config failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		log.Error("parse DSN failed", "err", err)
		os.Exit(1)
	}
	poolCfg.MaxConns = 4
	poolCfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		log.Error("connect pool failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPing()
	if err := pool.Ping(pingCtx); err != nil {
		log.Error("database ping failed", "err", err)
		os.Exit(1)
	}

	metrics := spread.NewMetrics()
	handler := spread.NewHandler(
		spread.NewStore(pool),
		spread.NewCache(historyTTL, time.Now),
		time.Now,
		pollMS,
		metrics,
	)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           spread.CORS(mux),
		ReadHeaderTimeout: 2 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Error("listen failed", "addr", cfg.Addr, "err", err)
		os.Exit(1)
	}
	log.Info("spread api listening", "addr", cfg.Addr, "pairs", "XAU-XAG,XPT-XPD")
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("spread api server failed", "err", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutdown: stopping spread api")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("spread api shutdown failed", "err", err)
	}
	log.Info("shutdown complete")
}
