// Command analyze builds a local daily report from the collector's read-only
// /api/v1/stats and /api/v1/rows endpoints. It never touches the database
// directly and stores only aggregate results.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"time"
)

const (
	defaultURL = "https://p01--weather-api--cn5bkwlzyqr9.code.run"
	pageSize   = 5000
	maxPages   = 100
)

type symbolStats struct {
	Symbol        string  `json:"symbol"`
	Rows          int64   `json:"rows"`
	FirstTS       int64   `json:"first_ts"`
	LastTS        int64   `json:"last_ts"`
	MaxGapSec     int64   `json:"max_gap_sec"`
	Duplicates    int64   `json:"duplicates"`
	BidLTAskRatio float64 `json:"bid_lt_ask_ratio"`
	CoveragePct   float64 `json:"coverage_pct"`
}

type statsResponse struct {
	From             int64         `json:"from"`
	To               int64         `json:"to"`
	Symbols          []symbolStats `json:"symbols"`
	Partitions       []string      `json:"partitions"`
	FuturePartitions int           `json:"future_partitions"`
	FutureOK         bool          `json:"future_ok"`
	AgedPartitions   []string      `json:"aged_partitions"`
}

type rawRow struct {
	TS  int64   `json:"ts"`
	Bid float64 `json:"bid"`
	Ask float64 `json:"ask"`
}

type rowsResponse struct {
	From       int64    `json:"from"`
	To         int64    `json:"to"`
	Limit      int      `json:"limit"`
	NextCursor int64    `json:"next_cursor,omitempty"`
	Rows       []rawRow `json:"rows"`
}

type spreadStats struct {
	Symbol     string  `json:"symbol"`
	Rows       int     `json:"rows"`
	MeanSpread float64 `json:"mean_spread"`
	Median     float64 `json:"median_spread"`
	P95        float64 `json:"p95_spread"`
	Min        float64 `json:"min_spread"`
	Max        float64 `json:"max_spread"`
	MeanBPS    float64 `json:"mean_spread_bps"`
}

// Report is the compact output of one analysis run.
type Report struct {
	Window   statsResponse `json:"window"`
	Spread   spreadStats   `json:"spread"`
	Warnings []string      `json:"warnings,omitempty"`
}

func getJSON(ctx context.Context, client *http.Client, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", target, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", target, err)
	}
	return nil
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// run fetches the summary and paged spread rows, then computes aggregates.
func run(ctx context.Context, client *http.Client, base string, from, to int64, symbol string) (Report, error) {
	if to <= from {
		return Report{}, fmt.Errorf("window must be positive")
	}
	report := Report{}
	if err := getJSON(ctx, client, base+"/api/v1/stats?from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10), &report.Window); err != nil {
		return Report{}, err
	}
	for i := range report.Window.Symbols {
		s := &report.Window.Symbols[i]
		if span := s.LastTS - s.FirstTS; span > 0 {
			s.CoveragePct = 100 * float64(s.Rows) / float64(span)
		}
	}

	values := []float64{}
	basisPoints := []float64{}
	cursor := int64(0)
	for page := 0; page < maxPages; page++ {
		u, err := url.Parse(base + "/api/v1/rows")
		if err != nil {
			return Report{}, err
		}
		q := u.Query()
		q.Set("symbol", symbol)
		q.Set("from", strconv.FormatInt(from, 10))
		q.Set("to", strconv.FormatInt(to, 10))
		q.Set("order", "asc")
		q.Set("limit", strconv.Itoa(pageSize))
		if cursor > 0 {
			q.Set("cursor", strconv.FormatInt(cursor, 10))
		}
		u.RawQuery = q.Encode()

		var pageResp rowsResponse
		if err := getJSON(ctx, client, u.String(), &pageResp); err != nil {
			return Report{}, err
		}
		for _, row := range pageResp.Rows {
			spread := row.Ask - row.Bid
			values = append(values, spread)
			if mid := (row.Ask + row.Bid) / 2; mid > 0 {
				basisPoints = append(basisPoints, 10000*spread/mid)
			}
		}
		cursor = pageResp.NextCursor
		if cursor == 0 {
			break
		}
	}
	if cursor != 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("stopped at %d pages with data remaining", maxPages))
	}

	report.Spread = spreadStats{Symbol: symbol, Rows: len(values)}
	if len(values) > 0 {
		sort.Float64s(values)
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		report.Spread.MeanSpread = sum / float64(len(values))
		report.Spread.Median = percentile(values, 0.5)
		report.Spread.P95 = percentile(values, 0.95)
		report.Spread.Min = values[0]
		report.Spread.Max = values[len(values)-1]
	}
	if len(basisPoints) > 0 {
		sum := 0.0
		for _, v := range basisPoints {
			sum += v
		}
		report.Spread.MeanBPS = sum / float64(len(basisPoints))
	}
	return report, nil
}

func main() {
	base := flag.String("url", envOr("RAWPROXY_URL", defaultURL), "proxy base URL")
	symbol := flag.String("symbol", "XAUUSDT", "symbol for spread statistics")
	days := flag.Int("days", 1, "window length in days")
	flag.Parse()

	to := time.Now().UTC().Unix()
	from := to - int64(*days)*86400
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second}
	report, err := run(ctx, client, *base, from, to, *symbol)
	if err != nil {
		fmt.Fprintln(os.Stderr, "analyze:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		fmt.Fprintln(os.Stderr, "analyze:", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
