package fake

import (
	"encoding/json"
	"math/big"
	"testing"
	"time"
)

func TestQuoteForExactDecimalStrings(t *testing.T) {
	for _, symbol := range []string{"XPTUSDT", "XPDUSDT", "XAUUSDT", "XAGUSDT", "XCUUSDT"} {
		q := QuoteFor(symbol)
		for _, s := range []string{q.Bid, q.BidQty, q.Ask, q.AskQty} {
			if _, ok := new(big.Rat).SetString(s); !ok {
				t.Fatalf("%s: %q is not an exact decimal string", symbol, s)
			}
		}
		bid, _ := new(big.Rat).SetString(q.Bid)
		ask, _ := new(big.Rat).SetString(q.Ask)
		if bid.Cmp(ask) >= 0 {
			t.Fatalf("%s: bid %s >= ask %s", symbol, q.Bid, q.Ask)
		}
	}
}

func TestFixtureEndpointsParse(t *testing.T) {
	var info struct {
		RateLimits []struct {
			Type  string `json:"rateLimitType"`
			Limit int    `json:"limit"`
		} `json:"rateLimits"`
		Symbols []struct {
			Symbol string `json:"symbol"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(exchangeInfoJSON([]string{"XPTUSDT"}), &info); err != nil {
		t.Fatal(err)
	}
	if len(info.RateLimits) != 1 || info.RateLimits[0].Limit != 2400 {
		t.Fatalf("bad rate limits: %+v", info.RateLimits)
	}
	if len(info.Symbols) != 1 || info.Symbols[0].Symbol != "XPTUSDT" {
		t.Fatalf("bad symbols: %+v", info.Symbols)
	}

	now := time.Now().UTC()
	var sched struct {
		MarketSchedules map[string]struct {
			Sessions []struct {
				Type string `json:"type"`
			} `json:"sessions"`
		} `json:"marketSchedules"`
	}
	if err := json.Unmarshal(scheduleJSON(now), &sched); err != nil {
		t.Fatal(err)
	}
	if len(sched.MarketSchedules["COMMODITY"].Sessions) != 3 {
		t.Fatalf("bad schedule: %+v", sched)
	}
}
