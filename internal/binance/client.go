// Package binance implements the small public REST surface used by the
// collector. All numeric market data remains a string until SQL casts it.
package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"metals-collector/internal/ops"
)

// ErrorKind is the low-cardinality label used by api_errors_total.
type ErrorKind string

const (
	KindHTTP     ErrorKind = "http"
	KindTimeout  ErrorKind = "timeout"
	KindJSON     ErrorKind = "json"
	KindMissing  ErrorKind = "missing_field"
	KindBan      ErrorKind = "ban"
	KindStale    ErrorKind = "stale"
	KindUnknown  ErrorKind = "unknown"
	KindPaused   ErrorKind = "paused"
	KindCapacity ErrorKind = "capacity"
)

// APIError carries a stable kind for metrics and error handling.
type APIError struct {
	Kind   ErrorKind
	Symbol string
	Status int
	Err    error
}

func (e *APIError) Error() string {
	if e.Err == nil {
		return string(e.Kind)
	}
	if e.Status != 0 {
		return fmt.Sprintf("%s (HTTP %d): %v", e.Kind, e.Status, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Kind, e.Err)
}

func (e *APIError) Unwrap() error { return e.Err }

// KindOf extracts the error kind from a wrapped APIError.
func KindOf(err error) ErrorKind {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Kind
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout
	}
	return KindUnknown
}

// Client is a shared, keep-alive HTTP client with IP-wide ban handling.
type Client struct {
	base   string
	http   *http.Client
	reg    *ops.Registry
	log    *slog.Logger
	mu     sync.Mutex
	pause  time.Time
	minute time.Time
	weight int64
	bans   atomic.Int64
}

func NewClient(baseURL string, timeout time.Duration, reg *ops.Registry, log *slog.Logger) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 16
	transport.MaxIdleConnsPerHost = 8
	transport.DialContext = (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext

	c := &Client{
		base:   strings.TrimRight(baseURL, "/"),
		http:   &http.Client{Transport: transport, Timeout: timeout},
		reg:    reg,
		log:    log,
		minute: time.Now().UTC().Truncate(time.Minute),
	}
	reg.Gauge("used_weight", nil)
	reg.Counter("api_bans_total", nil)
	return c
}

// Paused reports whether the IP-wide ban pause is still active.
func (c *Client) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.pause)
}

func (c *Client) PauseUntil() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pause
}

// UsedWeight is the locally computed weight used in the current minute.
// Response headers are not authoritative on shared egress IPs.
func (c *Client) UsedWeight() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.weight
}

func (c *Client) countWeight(now time.Time, weight int) {
	minute := now.UTC().Truncate(time.Minute)
	if !minute.Equal(c.minute) {
		c.minute = minute
		c.weight = 0
	}
	c.weight += int64(weight)
	c.reg.Gauge("used_weight", nil).Set(c.weight)
}

// Get performs an authenticated-by-IP public GET. Symbol may be empty for
// non-symbol endpoints; it is only used as a metric label.
func (c *Client) Get(ctx context.Context, symbol, path string, weight int) ([]byte, error) {
	now := time.Now()
	if c.Paused() {
		err := &APIError{Kind: KindPaused, Symbol: symbol}
		c.reg.Counter("api_errors_total", map[string]string{"symbol": symbol, "kind": string(KindPaused)}).Inc()
		return nil, err
	}
	c.mu.Lock()
	c.countWeight(now, weight)
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, &APIError{Kind: KindHTTP, Symbol: symbol, Err: err}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		kind := KindHTTP
		if errors.Is(err, context.DeadlineExceeded) || osTimeout(err) {
			kind = KindTimeout
		}
		apiErr := &APIError{Kind: kind, Symbol: symbol, Err: err}
		c.reg.Counter("api_errors_total", map[string]string{"symbol": symbol, "kind": string(kind)}).Inc()
		return nil, apiErr
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		apiErr := &APIError{Kind: KindHTTP, Symbol: symbol, Err: err}
		c.reg.Counter("api_errors_total", map[string]string{"symbol": symbol, "kind": string(KindHTTP)}).Inc()
		return nil, apiErr
	}

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 418 {
		d := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		c.mu.Lock()
		c.pause = time.Now().Add(d)
		c.mu.Unlock()
		c.bans.Add(1)
		c.reg.Counter("api_bans_total", nil).Inc()
		c.reg.Counter("api_errors_total", map[string]string{"symbol": symbol, "kind": string(KindBan)}).Inc()
		c.log.Warn("IP ban received; pausing all symbols", "status", resp.StatusCode, "retry_after", d.String())
		return nil, &APIError{Kind: KindBan, Symbol: symbol, Status: resp.StatusCode, Err: fmt.Errorf("banned for %s", d)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{Kind: KindHTTP, Symbol: symbol, Status: resp.StatusCode, Err: fmt.Errorf("unexpected status %s", resp.Status)}
		c.reg.Counter("api_errors_total", map[string]string{"symbol": symbol, "kind": string(KindHTTP)}).Inc()
		return nil, apiErr
	}
	return body, nil
}

// BookTicker is the single-symbol bookTicker response.
type BookTicker struct {
	Symbol   string
	BidPrice string
	BidQty   string
	AskPrice string
	AskQty   string
	TimeMS   int64
}

func (c *Client) BookTicker(ctx context.Context, symbol string) (*BookTicker, error) {
	path := "/fapi/v1/ticker/bookTicker?symbol=" + url.QueryEscape(symbol)
	body, err := c.Get(ctx, symbol, path, 2)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Symbol   string `json:"symbol"`
		BidPrice string `json:"bidPrice"`
		BidQty   string `json:"bidQty"`
		AskPrice string `json:"askPrice"`
		AskQty   string `json:"askQty"`
		Time     int64  `json:"time"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, c.fail(symbol, KindJSON, err)
	}
	if raw.Symbol == "" || raw.BidPrice == "" || raw.BidQty == "" || raw.AskPrice == "" || raw.AskQty == "" || raw.Time <= 0 {
		return nil, c.fail(symbol, KindMissing, fmt.Errorf("incomplete bookTicker: %s", string(body)))
	}
	return &BookTicker{
		Symbol:   raw.Symbol,
		BidPrice: raw.BidPrice,
		BidQty:   raw.BidQty,
		AskPrice: raw.AskPrice,
		AskQty:   raw.AskQty,
		TimeMS:   raw.Time,
	}, nil
}

// ServerTime fetches /fapi/v1/time.
func (c *Client) ServerTime(ctx context.Context) (int64, error) {
	body, err := c.Get(ctx, "", "/fapi/v1/time", 1)
	if err != nil {
		return 0, err
	}
	var raw struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, c.fail("", KindJSON, err)
	}
	if raw.ServerTime <= 0 {
		return 0, c.fail("", KindMissing, fmt.Errorf("missing serverTime"))
	}
	return raw.ServerTime, nil
}

// Session is one tradingSchedule window from the COMMODITY market.
type Session struct {
	Start time.Time
	End   time.Time
	Kind  string
}

// TradingSchedule fetches and flattens the commodity session windows.
func (c *Client) TradingSchedule(ctx context.Context) ([]Session, error) {
	body, err := c.Get(ctx, "", "/fapi/v1/tradingSchedule", 5)
	if err != nil {
		return nil, err
	}
	var raw struct {
		MarketSchedules map[string]struct {
			Sessions []struct {
				StartTime int64  `json:"startTime"`
				EndTime   int64  `json:"endTime"`
				Type      string `json:"type"`
			} `json:"sessions"`
		} `json:"marketSchedules"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, c.fail("", KindJSON, err)
	}
	commodity, ok := raw.MarketSchedules["COMMODITY"]
	if !ok {
		return nil, c.fail("", KindMissing, fmt.Errorf("COMMODITY schedule missing"))
	}
	out := make([]Session, 0, len(commodity.Sessions))
	for _, s := range commodity.Sessions {
		if s.Type != "REGULAR" && s.Type != "NO_TRADING" {
			continue
		}
		out = append(out, Session{
			Start: time.UnixMilli(s.StartTime).UTC(),
			End:   time.UnixMilli(s.EndTime).UTC(),
			Kind:  s.Type,
		})
	}
	return out, nil
}

func (c *Client) fail(symbol string, kind ErrorKind, err error) error {
	apiErr := &APIError{Kind: kind, Symbol: symbol, Err: err}
	c.reg.Counter("api_errors_total", map[string]string{"symbol": symbol, "kind": string(kind)}).Inc()
	return apiErr
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	const fallback = 5 * time.Second
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(value); err == nil && secs > 0 {
		d := time.Duration(secs) * time.Second
		if d > 5*time.Minute {
			return 5 * time.Minute
		}
		return d
	}
	if t, err := http.ParseTime(value); err == nil {
		if d := t.Sub(now); d > 0 {
			if d > 5*time.Minute {
				return 5 * time.Minute
			}
			return d
		}
	}
	return fallback
}

func osTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	var t timeout
	return errors.As(err, &t) && t.Timeout()
}
