package fake

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"time"
)

// Quote is the deterministic fixture for one symbol. Values use exactly
// eight decimals so they round-trip through numeric(20,8) as strings.
type Quote struct {
	Bid    string
	BidQty string
	Ask    string
	AskQty string
}

var baseCents = map[string]int64{
	"XPTUSDT": 129914,
	"XPDUSDT": 198025,
	"XAUUSDT": 419010,
	"XAGUSDT": 6234,
}

// QuoteFor returns stable prices for the four pilot symbols and deterministic
// values for any additional symbol (test T8).
func QuoteFor(symbol string) Quote {
	cents, ok := baseCents[symbol]
	if !ok {
		h := fnv.New32a()
		_, _ = h.Write([]byte(symbol))
		cents = 1000 + int64(h.Sum32()%100000)
	}
	return Quote{
		Bid:    formatCents(cents),
		BidQty: "0.01500000",
		Ask:    formatCents(cents + 1),
		AskQty: "0.02400000",
	}
}

func formatCents(cents int64) string {
	return fmt.Sprintf("%d.%02d000000", cents/100, cents%100)
}

type exchangeInfo struct {
	RateLimits []map[string]any `json:"rateLimits"`
	Symbols    []map[string]any `json:"symbols"`
}

func exchangeInfoJSON(symbols []string) []byte {
	doc := exchangeInfo{
		RateLimits: []map[string]any{{
			"rateLimitType": "REQUEST_WEIGHT", "interval": "MINUTE", "intervalNum": 1, "limit": 2400,
		}},
		Symbols: make([]map[string]any, 0, len(symbols)),
	}
	for _, s := range symbols {
		doc.Symbols = append(doc.Symbols, map[string]any{
			"symbol": s,
			"filters": []map[string]any{
				{"filterType": "PRICE_FILTER", "tickSize": "0.01000000"},
				{"filterType": "LOT_SIZE", "stepSize": "0.001"},
			},
		})
	}
	return mustJSON(doc)
}

func timeJSON(now time.Time) []byte {
	return mustJSON(map[string]any{"serverTime": now.UnixMilli()})
}

func scheduleJSON(now time.Time) []byte {
	day := 24 * time.Hour
	sessions := []map[string]any{
		{"startTime": now.Add(-day).UnixMilli(), "endTime": now.Add(-18 * time.Hour).UnixMilli(), "type": "REGULAR"},
		{"startTime": now.Add(-18 * time.Hour).UnixMilli(), "endTime": now.Add(-12 * time.Hour).UnixMilli(), "type": "NO_TRADING"},
		{"startTime": now.Add(-12 * time.Hour).UnixMilli(), "endTime": now.Add(12 * time.Hour).UnixMilli(), "type": "REGULAR"},
	}
	return mustJSON(map[string]any{
		"updateTime": now.UnixMilli(),
		"marketSchedules": map[string]any{
			"COMMODITY": map[string]any{"sessions": sessions},
		},
	})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
