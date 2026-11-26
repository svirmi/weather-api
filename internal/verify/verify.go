// Package verify runs SQL and metrics assertions against a running collector.
package verify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"metals-collector/internal/config"
	"metals-collector/internal/fake"
)

// Options controls verification windows and optional fixture checks.
type Options struct {
	Window        time.Duration
	Fixtures      bool
	PauseSec      int
	MetricsURL    string
	AheadDays     int
	RetentionDays int
	SkewMaxMS     int
}

type check struct {
	name   string
	ok     bool
	detail string
}

// Run executes all eight checks and writes a pass/fail table. It returns true
// when every check passes.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, opts Options, out io.Writer) bool {
	checks := []check{
		checkRows(ctx, pool, cfg, opts),
		checkDuplicates(ctx, pool, opts),
		checkPartitions(ctx, pool, opts),
		checkAged(ctx, pool, opts),
		checkFixtures(ctx, pool, cfg, opts),
		checkSpread(ctx, pool, opts),
		checkCoverage(ctx, pool, cfg, opts),
		checkSkew(ctx, opts),
	}

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tRESULT\tDETAIL")
	allOK := true
	for _, c := range checks {
		result := "PASS"
		if !c.ok {
			result = "FAIL"
			allOK = false
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.name, result, c.detail)
	}
	_ = tw.Flush()
	return allOK
}

func checkRows(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, opts Options) check {
	since := time.Now().Add(-opts.Window)
	rows, err := pool.Query(ctx, "SELECT symbol, count(*) FROM book_l1 WHERE ts_sec >= $1 GROUP BY symbol", since)
	if err != nil {
		return check{"rows_present", false, err.Error()}
	}
	counts := make(map[string]int64)
	for rows.Next() {
		var symbol string
		var n int64
		if err := rows.Scan(&symbol, &n); err != nil {
			rows.Close()
			return check{"rows_present", false, err.Error()}
		}
		counts[symbol] = n
	}
	rows.Close()
	missing := []string{}
	for _, s := range cfg.Symbols {
		if counts[s] == 0 {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return check{"rows_present", false, "no rows for " + strings.Join(missing, ",")}
	}
	return check{"rows_present", true, fmt.Sprintf("%d symbols present", len(cfg.Symbols))}
}

func checkDuplicates(ctx context.Context, pool *pgxpool.Pool, opts Options) check {
	since := time.Now().Add(-opts.Window)
	var n int64
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM (
		  SELECT 1 FROM book_l1 WHERE ts_sec >= $1
		  GROUP BY ts_sec, symbol HAVING count(*) > 1
		) d`, since).Scan(&n)
	if err != nil {
		return check{"no_duplicates", false, err.Error()}
	}
	return check{"no_duplicates", n == 0, fmt.Sprintf("%d duplicate keys", n)}
}

func checkPartitions(ctx context.Context, pool *pgxpool.Pool, opts Options) check {
	names, err := partitionNames(ctx, pool)
	if err != nil {
		return check{"future_partitions", false, err.Error()}
	}
	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Truncate(24 * time.Hour).Format("20060102")
	haveTomorrow := false
	future := 0
	for _, name := range names {
		if name == "book_l1_"+tomorrow {
			haveTomorrow = true
		}
		day, err := time.Parse("20060102", strings.TrimPrefix(name, "book_l1_"))
		if err == nil && day.After(time.Now().UTC().Truncate(24*time.Hour)) {
			future++
		}
	}
	ok := haveTomorrow && future >= opts.AheadDays
	return check{"future_partitions", ok, fmt.Sprintf("tomorrow=%v future=%d required=%d", haveTomorrow, future, opts.AheadDays)}
}

func checkAged(ctx context.Context, pool *pgxpool.Pool, opts Options) check {
	names, err := partitionNames(ctx, pool)
	if err != nil {
		return check{"no_aged_partitions", false, err.Error()}
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -opts.RetentionDays).Truncate(24 * time.Hour)
	aged := []string{}
	for _, name := range names {
		day, err := time.Parse("20060102", strings.TrimPrefix(name, "book_l1_"))
		if err == nil && !day.AddDate(0, 0, 1).After(cutoff) {
			aged = append(aged, name)
		}
	}
	return check{"no_aged_partitions", len(aged) == 0, fmt.Sprintf("aged=%v", aged)}
}

func checkFixtures(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, opts Options) check {
	if !opts.Fixtures {
		return check{"numeric_round_trip", true, "skipped (no --fixtures)"}
	}
	bad := []string{}
	for _, symbol := range cfg.Symbols {
		var bid, ask string
		err := pool.QueryRow(ctx, `
			SELECT bid::text, ask::text FROM book_l1
			WHERE symbol = $1 ORDER BY ts_sec DESC LIMIT 1`, symbol).Scan(&bid, &ask)
		if err != nil {
			bad = append(bad, symbol+":query")
			continue
		}
		q := fake.QuoteFor(symbol)
		if bid != q.Bid || ask != q.Ask {
			bad = append(bad, fmt.Sprintf("%s:%s/%s", symbol, bid, ask))
		}
	}
	return check{"numeric_round_trip", len(bad) == 0, fmt.Sprintf("mismatches=%v", bad)}
}

func checkSpread(ctx context.Context, pool *pgxpool.Pool, opts Options) check {
	since := time.Now().Add(-opts.Window)
	var good, total int64
	err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE bid < ask), count(*) FROM book_l1
		WHERE ts_sec >= $1`, since).Scan(&good, &total)
	if err != nil {
		return check{"bid_lt_ask", false, err.Error()}
	}
	if total == 0 {
		return check{"bid_lt_ask", false, "no rows"}
	}
	ratio := float64(good) / float64(total)
	return check{"bid_lt_ask", ratio >= 0.99, fmt.Sprintf("%.4f over %d rows", ratio, total)}
}

func checkCoverage(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, opts Options) check {
	since := time.Now().Add(-opts.Window)
	expected := opts.Window.Seconds() - float64(opts.PauseSec)
	if expected <= 0 {
		return check{"coverage", false, "window too small"}
	}
	rows, err := pool.Query(ctx, "SELECT symbol, count(*) FROM book_l1 WHERE ts_sec >= $1 GROUP BY symbol", since)
	if err != nil {
		return check{"coverage", false, err.Error()}
	}
	counts := make(map[string]int64)
	for rows.Next() {
		var s string
		var n int64
		if err := rows.Scan(&s, &n); err != nil {
			rows.Close()
			return check{"coverage", false, err.Error()}
		}
		counts[s] = n
	}
	rows.Close()
	lowest := 1.0
	var worst string
	for _, s := range cfg.Symbols {
		ratio := float64(counts[s]) / expected
		if ratio < lowest {
			lowest, worst = ratio, s
		}
	}
	return check{"coverage", lowest >= 0.995, fmt.Sprintf("min=%.4f (%s) expected=%.0fs", lowest, worst, expected)}
}

var metricLine = regexp.MustCompile(`(?m)^([a-z_]+)\s+(-?[0-9.]+)\s*$`)

func checkSkew(ctx context.Context, opts Options) check {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.MetricsURL, nil)
	if err != nil {
		return check{"fetch_skew", false, err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return check{"fetch_skew", false, err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return check{"fetch_skew", false, err.Error()}
	}
	values := make(map[string]float64)
	for _, m := range metricLine.FindAllStringSubmatch(string(body), -1) {
		v, err := strconv.ParseFloat(m[2], 64)
		if err == nil {
			values[m[1]] = v
		}
	}
	fetchSkew, ok := values["cycle_fetch_skew_ms"]
	if !ok {
		return check{"fetch_skew", false, "metric cycle_fetch_skew_ms missing"}
	}
	// exchange-time skew is informational: illiquid symbols (XPT) carry a
	// stale transaction time even when all legs were fetched together.
	exchangeSkew := values["cycle_skew_ms"]
	bound := float64(opts.SkewMaxMS)
	return check{"fetch_skew", fetchSkew <= bound,
		fmt.Sprintf("fetch=%.0fms bound=%.0fms exch_time=%.0fms (informational)", fetchSkew, bound, exchangeSkew)}
}

func partitionNames(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'book_l1'::regclass
		  AND c.relname ~ '^book_l1_[0-9]{8}$'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// Report prints a daily completeness summary. It fails on low coverage or
// dropped ticks, matching the locked report contract.
func Report(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, opts Options, out io.Writer) bool {
	coverage := checkCoverage(ctx, pool, cfg, opts)
	dropped := fetchMetric(ctx, opts.MetricsURL, "dropped_ticks_total")
	fmt.Fprintf(out, "daily completeness report (window=%s)\n", opts.Window)
	fmt.Fprintf(out, "coverage: %s\n", coverage.detail)
	fmt.Fprintf(out, "dropped_ticks_total: %.0f\n", dropped)
	return coverage.ok && dropped == 0
}

func fetchMetric(ctx context.Context, url, name string) float64 {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return -1
	}
	resp, err := client.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return -1
	}
	for _, m := range metricLine.FindAllStringSubmatch(string(body), -1) {
		if m[1] == name {
			v, _ := strconv.ParseFloat(m[2], 64)
			return v
		}
	}
	return -1
}
