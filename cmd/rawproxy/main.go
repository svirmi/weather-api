// Command rawproxy exposes bounded, read-only database rows for debugging.
// It is a separate service: it cannot write to book_l1 and never migrates.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"metals-collector/internal/rawproxy"
)

const (
	defaultAddr     = "127.0.0.1:9092"
	defaultOrigins  = "http://127.0.0.1:8080"
	shutdownTimeout = 5 * time.Second
)

type config struct {
	DSN     string
	Addr    string
	Token   string
	Origins string
}

func loadConfig() (config, error) {
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return config{}, errors.New("DB_DSN (or DATABASE_URL) is required")
	}
	token := os.Getenv("RAWPROXY_TOKEN")
	if token == "" {
		return config{}, errors.New("RAWPROXY_TOKEN is required")
	}
	addr := os.Getenv("RAWPROXY_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	origins := os.Getenv("RAWPROXY_ALLOWED_ORIGINS")
	if origins == "" {
		origins = defaultOrigins
	}
	return config{DSN: dsn, Addr: addr, Token: token, Origins: origins}, nil
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func healthcheckCommand() error {
	addr := os.Getenv("RAWPROXY_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("RAWPROXY_ADDR %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

func run() error {
	log := newLogger()
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return fmt.Errorf("parse DSN: %w", err)
	}
	poolCfg.MaxConns = 2
	poolCfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("connect pool: %w", err)
	}
	defer pool.Close()

	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPing()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}

	h := rawproxy.NewHandler(rawproxy.NewStore(pool), time.Now, rawproxy.NewMetrics())
	mux := http.NewServeMux()
	h.Register(mux)
	auth := rawproxy.NewAuth(cfg.Token)
	handler := rawproxy.NewCORS(cfg.Origins).Middleware(auth.Middleware(mux))
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 2 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Info("raw proxy listening", "addr", cfg.Addr, "max_rows", rawproxy.MaxRows, "max_window", rawproxy.MaxWindow.String())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("raw proxy server failed", "err", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutdown: stopping raw proxy")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("raw proxy shutdown failed", "err", err)
	}
	log.Info("shutdown complete")
	return nil
}

func main() {
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = strings.ToLower(os.Args[1])
	}
	var err error
	switch cmd {
	case "run":
		err = run()
	case "healthcheck":
		err = healthcheckCommand()
	default:
		err = fmt.Errorf("unknown subcommand %q (want run|healthcheck)", cmd)
	}
	if err != nil {
		newLogger().Error("command failed", "command", cmd, "err", err)
		os.Exit(1)
	}
}
