// Package session polls the commodity trading schedule into a side table.
package session

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"metals-collector/internal/binance"
)

const upsertSQL = `
INSERT INTO session_window (start_time, end_time, kind) VALUES ($1,$2,$3)
ON CONFLICT (start_time) DO UPDATE SET end_time=EXCLUDED.end_time, kind=EXCLUDED.kind`

// Run polls immediately and then hourly until ctx is cancelled.
func Run(ctx context.Context, client *binance.Client, pool *pgxpool.Pool, log *slog.Logger) error {
	if err := poll(ctx, client, pool, log); err != nil {
		log.Warn("initial tradingSchedule poll failed", "err", err)
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := poll(ctx, client, pool, log); err != nil {
				log.Warn("tradingSchedule poll failed", "err", err)
			}
		}
	}
}

func poll(ctx context.Context, client *binance.Client, pool *pgxpool.Pool, log *slog.Logger) error {
	sessions, err := client.TradingSchedule(ctx)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		return nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	batch := &pgx.Batch{}
	for _, s := range sessions {
		batch.Queue(upsertSQL, s.Start, s.End, s.Kind)
	}
	results := tx.SendBatch(ctx, batch)
	for range sessions {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return err
		}
	}
	if err := results.Close(); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	log.Info("commodity sessions stored", "windows", len(sessions))
	return nil
}
