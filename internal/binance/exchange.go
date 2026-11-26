package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// RateLimit mirrors one exchangeInfo.rateLimits entry.
type RateLimit struct {
	Type        string `json:"rateLimitType"`
	Interval    string `json:"interval"`
	IntervalNum int    `json:"intervalNum"`
	Limit       int    `json:"limit"`
}

// SymbolInfo carries the precision fields asserted at startup.
type SymbolInfo struct {
	Symbol   string
	TickSize string
	StepSize string
}

// ExchangeInfo is the subset of /fapi/v1/exchangeInfo we need.
type ExchangeInfo struct {
	RateLimits []RateLimit
	Symbols    map[string]SymbolInfo
}

func (c *Client) ExchangeInfo(ctx context.Context) (*ExchangeInfo, error) {
	body, err := c.Get(ctx, "", "/fapi/v1/exchangeInfo", 1)
	if err != nil {
		return nil, err
	}
	var raw struct {
		RateLimits []RateLimit `json:"rateLimits"`
		Symbols    []struct {
			Symbol  string `json:"symbol"`
			Filters []struct {
				FilterType string `json:"filterType"`
				TickSize   string `json:"tickSize"`
				StepSize   string `json:"stepSize"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, c.fail("", KindJSON, err)
	}
	out := &ExchangeInfo{RateLimits: raw.RateLimits, Symbols: make(map[string]SymbolInfo, len(raw.Symbols))}
	for _, s := range raw.Symbols {
		info := SymbolInfo{Symbol: s.Symbol}
		for _, f := range s.Filters {
			switch f.FilterType {
			case "PRICE_FILTER":
				info.TickSize = f.TickSize
			case "LOT_SIZE":
				info.StepSize = f.StepSize
			}
		}
		out.Symbols[s.Symbol] = info
	}
	return out, nil
}

// RequestWeightLimit returns the 1-minute REQUEST_WEIGHT ceiling.
func (e *ExchangeInfo) RequestWeightLimit() (int, error) {
	for _, r := range e.RateLimits {
		if r.Type == "REQUEST_WEIGHT" && r.Interval == "MINUTE" && r.IntervalNum == 1 {
			return r.Limit, nil
		}
	}
	return 0, fmt.Errorf("exchangeInfo has no REQUEST_WEIGHT/MINUTE limit")
}

// AssertStartup fails fast on a missing symbol, missing precision or an
// over-budget request weight.
func AssertStartup(ctx context.Context, c *Client, symbols []string, budget int, log *slog.Logger) error {
	info, err := c.ExchangeInfo(ctx)
	if err != nil {
		return fmt.Errorf("fetch exchangeInfo: %w", err)
	}
	limit, err := info.RequestWeightLimit()
	if err != nil {
		return err
	}
	if budget > limit {
		return fmt.Errorf("weight budget %d exceeds REQUEST_WEIGHT limit %d", budget, limit)
	}
	for _, symbol := range symbols {
		si, ok := info.Symbols[symbol]
		if !ok {
			return fmt.Errorf("symbol %s absent from exchangeInfo", symbol)
		}
		if si.TickSize == "" || si.StepSize == "" {
			return fmt.Errorf("symbol %s has incomplete PRICE_FILTER/LOT_SIZE", symbol)
		}
		if _, err := strconv.ParseFloat(si.TickSize, 64); err != nil {
			return fmt.Errorf("symbol %s tickSize %q is not numeric", symbol, si.TickSize)
		}
		log.Info("symbol precision", "symbol", symbol, "tick_size", si.TickSize, "step_size", si.StepSize)
	}
	serverMS, err := c.ServerTime(ctx)
	if err != nil {
		return fmt.Errorf("fetch server time: %w", err)
	}
	drift := serverMS - time.Now().UnixMilli()
	log.Info("startup checks passed", "weight_budget", budget, "weight_limit", limit, "clock_drift_ms", drift)
	return nil
}
