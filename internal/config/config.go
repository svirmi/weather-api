// Package config loads and validates the collector's environment-only configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// MaxSymbols is the largest symbol set that fits the REQUEST_WEIGHT budget
// (2400/min at the time of writing). 16 symbols cost ~1926 weight/min.
const MaxSymbols = 16

// Config is the complete runtime configuration.
type Config struct {
	Symbols            []string
	PollInterval       time.Duration
	BinanceBaseURL     string
	DBDSN              string
	FlushInterval      time.Duration
	BufferMaxMinutes   int
	HTTPTimeout        time.Duration
	RetentionDays      int
	PartitionAheadDays int
	MetricsAddr        string
	LogLevel           string
	SkewMaxMS          int
}

// Load reads configuration from the environment. requireDSN is false for
// subcommands that do not touch PostgreSQL (e.g. healthcheck).
func Load(requireDSN bool) (*Config, error) {
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL") // common managed-platform alias
	}
	c := &Config{
		BinanceBaseURL: os.Getenv("BINANCE_BASE_URL"),
		DBDSN:          dsn,
		MetricsAddr:    os.Getenv("METRICS_ADDR"),
		LogLevel:       os.Getenv("LOG_LEVEL"),
	}
	if c.BinanceBaseURL == "" {
		c.BinanceBaseURL = "https://fapi.binance.com"
	}
	if c.MetricsAddr == "" {
		c.MetricsAddr = ":9090"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}

	var errs []string
	c.Symbols = splitSymbols(envOr("SYMBOLS", "XPTUSDT,XPDUSDT,XAUUSDT,XAGUSDT"))

	var err error
	if c.PollInterval, err = envDurationMS("POLL_INTERVAL_MS", 1000); err != nil {
		errs = append(errs, err.Error())
	}
	if c.FlushInterval, err = envDurationMS("FLUSH_INTERVAL_MS", 2000); err != nil {
		errs = append(errs, err.Error())
	}
	if c.HTTPTimeout, err = envDurationMS("HTTP_TIMEOUT_MS", 3000); err != nil {
		errs = append(errs, err.Error())
	}
	if c.BufferMaxMinutes, err = envInt("BUFFER_MAX_MINUTES", 10); err != nil {
		errs = append(errs, err.Error())
	}
	if c.RetentionDays, err = envInt("RETENTION_DAYS", 45); err != nil {
		errs = append(errs, err.Error())
	}
	if c.PartitionAheadDays, err = envInt("PARTITION_AHEAD_DAYS", 7); err != nil {
		errs = append(errs, err.Error())
	}
	if c.SkewMaxMS, err = envInt("SKEW_MAX_MS", 250); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid config: %s", strings.Join(errs, "; "))
	}
	if err := c.Validate(requireDSN); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate enforces the invariants the collector relies on.
func (c *Config) Validate(requireDSN bool) error {
	if len(c.Symbols) == 0 {
		return fmt.Errorf("SYMBOLS must not be empty")
	}
	if len(c.Symbols) > MaxSymbols {
		return fmt.Errorf("SYMBOLS has %d entries; maximum is %d (REQUEST_WEIGHT budget)", len(c.Symbols), MaxSymbols)
	}
	seen := make(map[string]bool, len(c.Symbols))
	for _, s := range c.Symbols {
		if s == "" {
			return fmt.Errorf("SYMBOLS contains an empty entry")
		}
		if seen[s] {
			return fmt.Errorf("SYMBOLS contains duplicate %q", s)
		}
		seen[s] = true
	}
	if requireDSN && c.DBDSN == "" {
		return fmt.Errorf("DB_DSN is required")
	}
	if c.PollInterval <= 0 || c.FlushInterval <= 0 || c.HTTPTimeout <= 0 {
		return fmt.Errorf("POLL_INTERVAL_MS, FLUSH_INTERVAL_MS and HTTP_TIMEOUT_MS must be positive")
	}
	if c.BufferMaxMinutes <= 0 {
		return fmt.Errorf("BUFFER_MAX_MINUTES must be positive")
	}
	if c.RetentionDays <= 0 {
		return fmt.Errorf("RETENTION_DAYS must be positive")
	}
	if c.PartitionAheadDays < 0 {
		return fmt.Errorf("PARTITION_AHEAD_DAYS must not be negative")
	}
	if c.SkewMaxMS < 0 {
		return fmt.Errorf("SKEW_MAX_MS must not be negative")
	}
	if c.MetricsAddr == "" {
		return fmt.Errorf("METRICS_ADDR must not be empty")
	}
	return nil
}

// WeightBudget is the worst-case request weight per minute: N concurrent
// bookTicker calls (weight 2) every second, plus /time (1) and the hourly
// tradingSchedule (5) charged in the same minute.
func (c *Config) WeightBudget() int {
	return len(c.Symbols)*2*60 + 1 + 5
}

func splitSymbols(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q", key, raw)
	}
	return v, nil
}

func envDurationMS(key string, def int) (time.Duration, error) {
	v, err := envInt(key, def)
	if err != nil {
		return 0, err
	}
	return time.Duration(v) * time.Millisecond, nil
}
