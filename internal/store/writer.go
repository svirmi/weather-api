package store

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"metals-collector/internal/model"
	"metals-collector/internal/ops"
)

// flushTimeout caps a single flush attempt (including partition repair) so a
// blocked pool checkout cannot stall the writer goroutine indefinitely.
const flushTimeout = 3 * time.Second

const insertSQL = `
INSERT INTO book_l1 (ts_sec, symbol, exch_time, bid, bid_qty, ask, ask_qty)
VALUES ($1,$2,$3,$4::numeric,$5::numeric,$6::numeric,$7::numeric)
ON CONFLICT (ts_sec, symbol) DO NOTHING`

// Ring is a bounded FIFO of rows awaiting insertion. Overflow drops the
// oldest rows and counts them.
type Ring struct {
	rows    []model.Row
	head    int
	cap     int
	dropped int64
}

func NewRing(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{cap: capacity}
}

func (r *Ring) Push(row model.Row) {
	if r.Len() >= r.cap {
		r.head++
		r.dropped++
	}
	r.rows = append(r.rows, row)
	// Compaction must happen on the push path too: during a database outage
	// Ack is never called, so without this the slice would grow with outage
	// duration instead of staying near cap.
	r.compact()
}

// compact drops acknowledged rows from the front once they dominate the
// backing slice. It is safe to call from both Push and Ack.
func (r *Ring) compact() {
	if r.head >= r.cap && r.head*2 >= len(r.rows) {
		copy(r.rows, r.rows[r.head:])
		r.rows = r.rows[:len(r.rows)-r.head]
		r.head = 0
	}
}

func (r *Ring) Len() int { return len(r.rows) - r.head }

func (r *Ring) Dropped() int64 { return r.dropped }

func (r *Ring) Snapshot() []model.Row {
	return append([]model.Row(nil), r.rows[r.head:]...)
}

func (r *Ring) Ack(n int) {
	if n <= 0 {
		return
	}
	r.head += n
	if r.head >= len(r.rows) {
		r.head = 0
		r.rows = r.rows[:0]
		return
	}
	r.compact()
}

// Writer batches rows into one transaction per flush. A failed flush keeps
// its rows at the head of the ring for the next attempt.
type Writer struct {
	pool          *pgxpool.Pool
	mgr           *Manager
	flushInterval time.Duration
	in            chan model.Row
	ring          *Ring
	reg           *ops.Registry
	log           *slog.Logger
	done          chan struct{}
	lastDropped   int64
}

func NewWriter(pool *pgxpool.Pool, mgr *Manager, flushInterval time.Duration, capacity int, reg *ops.Registry, log *slog.Logger) *Writer {
	return &Writer{
		pool:          pool,
		mgr:           mgr,
		flushInterval: flushInterval,
		in:            make(chan model.Row, 512),
		ring:          NewRing(capacity),
		reg:           reg,
		log:           log,
		done:          make(chan struct{}),
	}
}

func (w *Writer) In() chan<- model.Row { return w.in }

func (w *Writer) Buffered() int { return w.ring.Len() }

// Run consumes rows until ctx is cancelled. It never flushes with a cancelled
// context; Stop performs the final flush with a fresh context.
func (w *Writer) Run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case row := <-w.in:
			w.push(row)
			if w.ring.Len() >= 64 {
				w.flush(ctx)
			}
		case <-ticker.C:
			w.flush(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// Stop waits for the writer goroutine, drains the input channel and performs
// a final flush using flushCtx.
func (w *Writer) Stop(flushCtx context.Context) error {
	<-w.done
	for {
		select {
		case row := <-w.in:
			w.push(row)
		default:
			return w.flush(flushCtx)
		}
	}
}

func (w *Writer) push(row model.Row) {
	w.ring.Push(row)
	w.reg.Gauge("buffer_rows", nil).Set(int64(w.ring.Len()))
	if d := w.ring.Dropped() - w.lastDropped; d > 0 {
		w.reg.Counter("dropped_ticks_total", nil).Add(d)
		w.lastDropped = w.ring.Dropped()
		w.log.Warn("buffer full; oldest rows dropped", "dropped", d)
	}
}

func (w *Writer) flush(ctx context.Context) error {
	rows := w.ring.Snapshot()
	if len(rows) == 0 {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	flushCtx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	start := time.Now()
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		err = w.insert(flushCtx, rows)
		if err == nil {
			break
		}
		if !isMissingPartition(err) || w.mgr == nil || flushCtx.Err() != nil {
			break
		}
		w.log.Warn("missing partition; repairing", "ts", rows[0].TS)
		if repairErr := w.mgr.Repair(flushCtx, rows[0].TS); repairErr != nil {
			return repairErr
		}
	}
	if err != nil {
		w.log.Error("flush failed; rows retained", "err", err, "buffered", w.ring.Len())
		return err
	}
	w.ring.Ack(len(rows))
	w.reg.Counter("inserted_rows_total", nil).Add(int64(len(rows)))
	w.reg.Gauge("buffer_rows", nil).Set(int64(w.ring.Len()))
	w.reg.Gauge("db_flush_seconds", nil).SetNanos(int64(time.Since(start)))
	return nil
}

func (w *Writer) insert(ctx context.Context, rows []model.Row) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(insertSQL, r.TS, r.Symbol, r.ExchTime, r.Bid, r.BidQty, r.Ask, r.AskQty)
	}
	results := tx.SendBatch(ctx, batch)
	var firstErr error
	for range rows {
		if _, err := results.Exec(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := results.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		return firstErr
	}
	return tx.Commit(ctx)
}

func isMissingPartition(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" &&
		strings.Contains(pgErr.Message, "no partition of relation")
}
