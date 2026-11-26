// Command collector is the single binary for the metals L1 pilot.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"metals-collector/internal/binance"
	"metals-collector/internal/config"
	"metals-collector/internal/coordinator"
	"metals-collector/internal/fake"
	"metals-collector/internal/migrate"
	"metals-collector/internal/ops"
	"metals-collector/internal/session"
	"metals-collector/internal/store"
	"metals-collector/internal/verify"
)

func main() {
	logger := newLogger()
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	args := subcommandArgs()
	var err error
	switch cmd {
	case "bootstrap":
		err = serve(args, true, logger)
	case "run":
		// The default invocation must self-initialise: platforms that use the
		// Dockerfile ENTRYPOINT without a CMD pass no subcommand at all.
		err = serve(args, true, logger)
	case "migrate":
		err = migrateCommand(args, logger)
	case "healthcheck":
		err = healthcheckCommand(args, logger)
	case "retention":
		err = retentionCommand(args, logger)
	case "fake":
		err = fakeCommand(args, logger)
	case "verify":
		err = verifyCommand(args, logger)
	case "report":
		err = reportCommand(args, logger)
	default:
		err = fmt.Errorf("unknown subcommand %q (want bootstrap|run|migrate|healthcheck|retention|fake|verify|report)", cmd)
	}
	if err != nil {
		logger.Error("command failed", "command", cmd, "err", err)
		os.Exit(1)
	}
}

// subcommandArgs returns the arguments after the subcommand. It is safe when
// the platform invokes the binary with no arguments at all.
func subcommandArgs() []string {
	if len(os.Args) > 2 {
		return os.Args[2:]
	}
	return nil
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func serve(_ []string, doMigrate bool, log *slog.Logger) error {
	cfg, err := config.Load(true)
	if err != nil {
		return err
	}
	log.Info("starting collector",
		"symbols", strings.Join(cfg.Symbols, ","),
		"weight_budget_per_min", cfg.WeightBudget(),
		"retention_days", cfg.RetentionDays)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("connect pool: %w", err)
	}
	defer pool.Close()
	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPing()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	if doMigrate {
		if err := migrate.Run(ctx, pool); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	mgr := store.NewManager(pool, cfg.RetentionDays, cfg.PartitionAheadDays, log)
	if err := mgr.Ensure(ctx); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}

	reg := registerMetrics(cfg)
	opsSrv := ops.NewServer(cfg.MetricsAddr, reg, log)
	if err := opsSrv.Start(); err != nil {
		return fmt.Errorf("start ops server: %w", err)
	}

	client := binance.NewClient(cfg.BinanceBaseURL, cfg.HTTPTimeout, reg, log)
	var startupErr error
	for attempt := 0; attempt < 15; attempt++ {
		startupErr = binance.AssertStartup(ctx, client, cfg.Symbols, cfg.WeightBudget(), log)
		if startupErr == nil {
			break
		}
		log.Warn("startup assertions failed; retrying", "attempt", attempt+1, "err", startupErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if startupErr != nil {
		return fmt.Errorf("startup assertions: %w", startupErr)
	}

	pipelineCtx, cancelPipeline := context.WithCancel(ctx)
	defer cancelPipeline()
	capacity := cfg.BufferMaxMinutes * 60 * len(cfg.Symbols)
	writer := store.NewWriter(pool, mgr, cfg.FlushInterval, capacity, reg, log)
	coord := coordinator.New(cfg, client, writer.In(), reg, log)

	coordDone, writerDone := make(chan struct{}), make(chan struct{})
	sessionDone, clockDone, mgrDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(coordDone)
		if err := coord.Run(pipelineCtx); err != nil {
			log.Error("coordinator stopped", "err", err)
		}
	}()
	go func() { defer close(writerDone); writer.Run(pipelineCtx) }()
	go func() {
		defer close(sessionDone)
		if err := session.Run(pipelineCtx, client, pool, log); err != nil {
			log.Error("session poller stopped", "err", err)
		}
	}()
	go func() { defer close(clockDone); runClock(pipelineCtx, client, reg, log) }()
	go func() {
		defer close(mgrDone)
		if err := mgr.Run(pipelineCtx, time.Hour); err != nil && pipelineCtx.Err() == nil {
			log.Error("partition manager stopped", "err", err)
		}
	}()

	log.Info("collector ready")
	<-ctx.Done()

	log.Info("shutdown: stopping intake")
	cancelPipeline()
	opsSrv.SetHealthy(false)
	<-coordDone // in-flight HTTP is bounded by HTTP_TIMEOUT_MS
	<-writerDone
	<-sessionDone
	<-clockDone
	<-mgrDone

	// Final flush must use a fresh context: ctx is already cancelled.
	fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := writer.Stop(fctx); err != nil {
		log.Error("final flush failed", "err", err, "buffered", writer.Buffered())
	}
	if err := opsSrv.Shutdown(fctx); err != nil {
		log.Error("ops shutdown failed", "err", err)
	}
	log.Info("shutdown complete", "buffered", writer.Buffered())
	return nil
}

func runClock(ctx context.Context, client *binance.Client, reg *ops.Registry, log *slog.Logger) {
	gauge := reg.Gauge("clock_drift_ms", nil)
	check := func() {
		serverMS, err := client.ServerTime(ctx)
		if err != nil {
			log.Warn("clock check failed", "err", err)
			return
		}
		drift := serverMS - time.Now().UnixMilli()
		gauge.Set(drift)
		if drift > 1000 || drift < -1000 {
			log.Warn("clock drift above 1s", "drift_ms", drift)
		}
	}
	check()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func registerMetrics(cfg *config.Config) *ops.Registry {
	reg := ops.NewRegistry()
	for _, s := range cfg.Symbols {
		reg.Counter("ticks_total", map[string]string{"symbol": s})
		reg.Counter("api_errors_total", map[string]string{"symbol": s, "kind": "http"})
	}
	reg.Counter("api_bans_total", nil)
	reg.Gauge("used_weight", nil)
	reg.Gauge("buffer_rows", nil)
	reg.Counter("dropped_ticks_total", nil)
	reg.Counter("inserted_rows_total", nil)
	reg.Gauge("db_flush_seconds", nil)
	reg.Gauge("cycle_skew_ms", nil)
	reg.Gauge("cycle_fetch_skew_ms", nil)
	reg.Gauge("clock_drift_ms", nil)
	return reg
}

func migrateCommand(_ []string, log *slog.Logger) error {
	cfg, err := config.Load(true)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := migrate.Run(ctx, pool); err != nil {
		return err
	}
	mgr := store.NewManager(pool, cfg.RetentionDays, cfg.PartitionAheadDays, log)
	if err := mgr.Ensure(ctx); err != nil {
		return err
	}
	log.Info("migration complete", "ahead_days", cfg.PartitionAheadDays)
	return nil
}

func healthcheckCommand(_ []string, _ *slog.Logger) error {
	addr := os.Getenv("METRICS_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("METRICS_ADDR %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}

func retentionCommand(args []string, log *slog.Logger) error {
	for _, a := range args {
		if a != "--once" {
			return fmt.Errorf("unknown retention flag %q", a)
		}
	}
	cfg, err := config.Load(true)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	mgr := store.NewManager(pool, cfg.RetentionDays, cfg.PartitionAheadDays, log)
	if err := mgr.Reconcile(ctx); err != nil {
		return err
	}
	log.Info("retention complete", "retention_days", cfg.RetentionDays)
	return nil
}

func fakeCommand(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("fake", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	scenario := fs.String("scenario", "normal", "fault scenario")
	if err := fs.Parse(args); err != nil {
		return err
	}
	scenarioName, err := fake.ParseScenario(*scenario)
	if err != nil {
		return err
	}
	cfg, err := config.Load(false)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return fake.Run(ctx, *addr, scenarioName, cfg.Symbols, log)
}

func verifyCommand(args []string, _ *slog.Logger) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	windowSec := fs.Int("window-sec", 180, "verification window in seconds")
	fixtures := fs.Bool("fixtures", false, "check prices against built-in fixtures")
	pauseSec := fs.Int("pause-sec", 0, "seconds excluded for ban pauses")
	metricsURL := fs.String("metrics-url", "", "collector metrics URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(true)
	if err != nil {
		return err
	}
	pool, ctx, closeFn, err := poolForCommand(cfg)
	if err != nil {
		return err
	}
	defer closeFn()
	opts := verify.Options{
		Window:        time.Duration(*windowSec) * time.Second,
		Fixtures:      *fixtures,
		PauseSec:      *pauseSec,
		MetricsURL:    metricsAddr(*metricsURL, cfg.MetricsAddr),
		AheadDays:     cfg.PartitionAheadDays,
		RetentionDays: cfg.RetentionDays,
		SkewMaxMS:     cfg.SkewMaxMS,
	}
	if !verify.Run(ctx, pool, cfg, opts, os.Stdout) {
		return fmt.Errorf("verification failed")
	}
	return nil
}

func reportCommand(args []string, _ *slog.Logger) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	windowSec := fs.Int("window-sec", 180, "report window in seconds")
	metricsURL := fs.String("metrics-url", "", "collector metrics URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(true)
	if err != nil {
		return err
	}
	pool, ctx, closeFn, err := poolForCommand(cfg)
	if err != nil {
		return err
	}
	defer closeFn()
	opts := verify.Options{
		Window:        time.Duration(*windowSec) * time.Second,
		MetricsURL:    metricsAddr(*metricsURL, cfg.MetricsAddr),
		AheadDays:     cfg.PartitionAheadDays,
		RetentionDays: cfg.RetentionDays,
		SkewMaxMS:     cfg.SkewMaxMS,
	}
	if !verify.Report(ctx, pool, cfg, opts, os.Stdout) {
		return fmt.Errorf("report thresholds not met")
	}
	return nil
}

func poolForCommand(cfg *config.Config) (*pgxpool.Pool, context.Context, func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	pool, err := pgxpool.New(ctx, cfg.DBDSN)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		cancel()
		pool.Close()
		return nil, nil, nil, err
	}
	return pool, ctx, func() { cancel(); pool.Close() }, nil
}

func metricsAddr(override, listenAddr string) string {
	if override != "" {
		return override
	}
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "http://127.0.0.1:9090/metrics"
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/metrics"
}
