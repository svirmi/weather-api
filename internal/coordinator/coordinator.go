// Package coordinator drives second-aligned, concurrent per-symbol polling.
package coordinator

import (
	"context"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"metals-collector/internal/binance"
	"metals-collector/internal/config"
	"metals-collector/internal/model"
	"metals-collector/internal/ops"
)

// Coordinator fans out one request per symbol from a single cycle boundary.
type Coordinator struct {
	cfg    *config.Config
	client *binance.Client
	out    chan<- model.Row
	reg    *ops.Registry
	log    *slog.Logger
}

func New(cfg *config.Config, client *binance.Client, out chan<- model.Row, reg *ops.Registry, log *slog.Logger) *Coordinator {
	return &Coordinator{cfg: cfg, client: client, out: out, reg: reg, log: log}
}

// Run polls until ctx is cancelled. Missed boundaries are skipped, never
// replayed: a slow cycle cannot produce a burst of catches-up samples.
func (c *Coordinator) Run(ctx context.Context) error {
	now := time.Now().UTC()
	next := time.Unix(now.Unix(), 0).Add(c.cfg.PollInterval)
	for !next.After(now) {
		next = next.Add(c.cfg.PollInterval)
	}
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			ts := time.Now().UTC().Truncate(time.Second)
			c.cycle(ctx, ts)
			next = ts.Add(c.cfg.PollInterval)
			now = time.Now()
			for !next.After(now) {
				next = next.Add(c.cfg.PollInterval)
			}
			timer.Reset(time.Until(next))
		}
	}
}

func (c *Coordinator) cycle(ctx context.Context, ts time.Time) {
	if c.client.Paused() {
		c.log.Debug("cycle skipped: IP pause active")
		return
	}
	fetchCtx, cancel := context.WithTimeout(ctx, c.cfg.HTTPTimeout)
	defer cancel()

	symbols := c.cfg.Symbols
	results := make([]*binance.BookTicker, len(symbols))
	errs := make([]error, len(symbols))
	doneAt := make([]time.Time, len(symbols))
	g, gctx := errgroup.WithContext(fetchCtx)
	for i, symbol := range symbols {
		i, symbol := i, symbol
		g.Go(func() error {
			bt, err := c.client.BookTicker(gctx, symbol)
			results[i], errs[i] = bt, err
			doneAt[i] = time.Now()
			return nil // a failed leg must not cancel its siblings
		})
	}
	_ = g.Wait()

	// Bans are IP-scoped: if any leg was banned, publish nothing for this
	// cycle rather than emitting a misleadingly incomplete set.
	for _, err := range errs {
		if err != nil && binance.KindOf(err) == binance.KindBan {
			c.log.Warn("cycle dropped: ban signalled by at least one symbol")
			return
		}
	}

	var minMS, maxMS int64
	haveTimes := false
	var firstDone, lastDone time.Time
	for i, bt := range results {
		if !doneAt[i].IsZero() {
			if firstDone.IsZero() || doneAt[i].Before(firstDone) {
				firstDone = doneAt[i]
			}
			if doneAt[i].After(lastDone) {
				lastDone = doneAt[i]
			}
		}
		if bt == nil || errs[i] != nil {
			continue
		}
		row := model.Row{
			TS:       ts,
			Symbol:   bt.Symbol,
			ExchTime: time.UnixMilli(bt.TimeMS).UTC(),
			Bid:      bt.BidPrice,
			BidQty:   bt.BidQty,
			Ask:      bt.AskPrice,
			AskQty:   bt.AskQty,
		}
		select {
		case c.out <- row:
			c.reg.Counter("ticks_total", map[string]string{"symbol": row.Symbol}).Inc()
		default:
			c.reg.Counter("dropped_ticks_total", nil).Inc()
			c.log.Warn("writer channel full; tick dropped", "symbol", row.Symbol)
		}
		if !haveTimes {
			minMS, maxMS = bt.TimeMS, bt.TimeMS
			haveTimes = true
		} else if bt.TimeMS < minMS {
			minMS = bt.TimeMS
		} else if bt.TimeMS > maxMS {
			maxMS = bt.TimeMS
		}
	}
	if haveTimes {
		c.reg.Gauge("cycle_skew_ms", nil).Set(maxMS - minMS)
	}
	if !firstDone.IsZero() {
		// Wall-clock spread of the concurrent GETs: this is the evidence that
		// legs were sampled together. exchange-time skew above is instead a
		// liquidity/staleness signal for slow metals such as XPT.
		c.reg.Gauge("cycle_fetch_skew_ms", nil).Set(lastDone.Sub(firstDone).Milliseconds())
	}
}
