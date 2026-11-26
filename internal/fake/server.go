// Package fake serves Binance-shaped fixtures for offline pipeline tests.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

type handler struct {
	scenario  string
	symbols   []string
	calls     atomic.Int64
	ban418Hit atomic.Bool
	log       *slog.Logger
}

// Handler returns the HTTP handler for a scenario.
func Handler(scenario string, symbols []string, log *slog.Logger) http.Handler {
	return &handler{scenario: scenario, symbols: symbols, log: log}
}

// Run starts the fake server and stops it when ctx is cancelled.
func Run(ctx context.Context, addr, scenario string, symbols []string, log *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: Handler(scenario, symbols, log), ReadHeaderTimeout: 2 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("fake server failed", "err", err)
		}
	}()
	log.Info("fake server listening", "addr", addr, "scenario", scenario)
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	switch r.URL.Path {
	case "/fapi/v1/exchangeInfo":
		writeJSON(w, http.StatusOK, exchangeInfoJSON(h.symbols))
	case "/fapi/v1/time":
		writeJSON(w, http.StatusOK, timeJSON(now))
	case "/fapi/v1/tradingSchedule":
		writeJSON(w, http.StatusOK, scheduleJSON(now))
	case "/fapi/v1/ticker/bookTicker":
		h.bookTicker(w, r, now)
	default:
		http.NotFound(w, r)
	}
}

func (h *handler) bookTicker(w http.ResponseWriter, r *http.Request, now time.Time) {
	symbol := r.URL.Query().Get("symbol")
	callNo := h.calls.Add(1)

	switch h.scenario {
	case "ban429":
		if callNo%10 == 0 {
			w.Header().Set("Retry-After", "5")
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"code": -1003, "msg": "Too many requests"})
			return
		}
	case "ban418":
		if h.ban418Hit.CompareAndSwap(false, true) {
			w.Header().Set("Retry-After", "5")
			writeJSON(w, 418, map[string]any{"code": -1003, "msg": "banned"})
			return
		}
	case "timeout":
		time.Sleep(5 * time.Second)
	case "garbage":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not json`))
		return
	case "missing":
		if len(h.symbols) > 0 && symbol == h.symbols[0] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": -1121, "msg": "Invalid symbol."})
			return
		}
	}

	q := QuoteFor(symbol)
	timeMS := now.UnixMilli()
	if h.scenario == "stale" {
		timeMS = now.Add(-30 * time.Second).UnixMilli()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"symbol":       symbol,
		"bidPrice":     q.Bid,
		"bidQty":       q.BidQty,
		"askPrice":     q.Ask,
		"askQty":       q.AskQty,
		"time":         timeMS,
		"lastUpdateId": callNo,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if raw, ok := body.([]byte); ok {
		_, _ = w.Write(raw)
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// ParseScenario validates a scenario name.
func ParseScenario(name string) (string, error) {
	switch name {
	case "normal", "ban429", "ban418", "timeout", "garbage", "missing", "stale":
		return name, nil
	default:
		return "", errors.New("unknown scenario: " + name)
	}
}
