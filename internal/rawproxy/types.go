// Package rawproxy exposes tightly bounded, read-only rows from the pilot
// database for debugging and test tooling. It never accepts client SQL.
package rawproxy

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

const (
	MaxRows      = 5000
	MaxWindow    = 24 * time.Hour
	DefaultRows  = 1000
	QueryTimeout = 10 * time.Second
)

// Symbols is the allow-list of queryable symbols.
var Symbols = map[string]bool{
	"XAUUSDT": true,
	"XAGUSDT": true,
	"XPTUSDT": true,
	"XPDUSDT": true,
}

// Row is one raw book_l1 observation. Timestamps are epoch seconds.
type Row struct {
	TS       int64   `json:"ts"`
	ExchTime int64   `json:"exchTime"`
	Bid      float64 `json:"bid"`
	BidQty   float64 `json:"bidQty"`
	Ask      float64 `json:"ask"`
	AskQty   float64 `json:"askQty"`
}

// SpreadRow is one row of the book_l1_spread view.
type SpreadRow struct {
	TS     int64   `json:"ts"`
	Symbol string  `json:"symbol"`
	Bid    float64 `json:"bid"`
	Ask    float64 `json:"ask"`
	BidQty float64 `json:"bidQty"`
	AskQty float64 `json:"askQty"`
	Spread float64 `json:"spread"`
	Mid    float64 `json:"mid"`
}

// SessionRow is one session_window row.
type SessionRow struct {
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	Kind  string `json:"kind"`
}

// Query is the validated, bounded filter shared by all endpoints.
type Query struct {
	Symbol string
	From   int64
	To     int64
	Limit  int
	Order  string
}

// ParseQuery validates a request's filters. from/to default to the last 24 h.
func ParseQuery(v url.Values, now time.Time, requireSymbol bool) (Query, error) {
	to := now.UTC().Unix()
	from := to - int64(MaxWindow/time.Second)
	var err error
	if raw := v.Get("to"); raw != "" {
		to, err = atoi64(raw)
		if err != nil {
			return Query{}, fmt.Errorf("invalid to: %s", raw)
		}
	}
	if raw := v.Get("from"); raw != "" {
		from, err = atoi64(raw)
		if err != nil {
			return Query{}, fmt.Errorf("invalid from: %s", raw)
		}
	}
	if to <= from || time.Duration(to-from)*time.Second > MaxWindow {
		return Query{}, fmt.Errorf("window must be positive and at most 24h")
	}

	limit := DefaultRows
	if raw := v.Get("limit"); raw != "" {
		n, err := atoi64(raw)
		if err != nil || n < 1 || n > MaxRows {
			return Query{}, fmt.Errorf("limit must be between 1 and %d", MaxRows)
		}
		limit = int(n)
	}

	order := v.Get("order")
	if order == "" {
		order = "desc"
	}
	if order != "asc" && order != "desc" {
		return Query{}, fmt.Errorf("order must be asc or desc")
	}

	symbol := v.Get("symbol")
	if symbol != "" && !Symbols[symbol] {
		return Query{}, fmt.Errorf("unknown symbol: %s", symbol)
	}
	if requireSymbol && symbol == "" {
		return Query{}, fmt.Errorf("symbol is required")
	}
	return Query{Symbol: symbol, From: from, To: to, Limit: limit, Order: order}, nil
}

func atoi64(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not an integer")
	}
	return n, nil
}
