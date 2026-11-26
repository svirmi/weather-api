// Package store owns PostgreSQL access: schema migrations, partition
// lifecycle, batched writes and the in-memory retry buffer.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Manager creates and drops daily book_l1 partitions. All DDL is serialized
// through mu so that the nightly task and an insert-repair path cannot race.
type Manager struct {
	pool          *pgxpool.Pool
	retentionDays int
	aheadDays     int
	log           *slog.Logger
	mu            sync.Mutex
}

func NewManager(pool *pgxpool.Pool, retentionDays, aheadDays int, log *slog.Logger) *Manager {
	return &Manager{pool: pool, retentionDays: retentionDays, aheadDays: aheadDays, log: log}
}

// Ensure creates today's partition and PARTITION_AHEAD_DAYS future ones.
func (m *Manager) Ensure(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureLocked(ctx)
}

func (m *Manager) ensureLocked(ctx context.Context) error {
	today := todayUTC()
	for d := today; !d.After(today.AddDate(0, 0, m.aheadDays)); d = d.AddDate(0, 0, 1) {
		if err := m.createLocked(ctx, d); err != nil {
			return fmt.Errorf("ensure partition %s: %w", partitionName(d), err)
		}
	}
	return nil
}

// DropAged drops only partitions whose entire UTC day ends before the
// retention cutoff. Today's partition is never a candidate.
func (m *Manager) DropAged(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dropAgedLocked(ctx)
}

func (m *Manager) dropAgedLocked(ctx context.Context) error {
	now := time.Now().UTC()
	cutoff := now.AddDate(0, 0, -m.retentionDays).Truncate(24 * time.Hour)

	// Enumerate real partitions rather than a window near the cutoff: a
	// backdated partition can arbitrarily predate it (see test T7).
	rows, err := m.pool.Query(ctx, `
		SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'book_l1'::regclass
		  AND c.relname ~ '^book_l1_[0-9]{8}$'`)
	if err != nil {
		return fmt.Errorf("list partitions: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, name := range names {
		day, err := time.Parse("20060102", strings.TrimPrefix(name, "book_l1_"))
		if err != nil {
			continue
		}
		if day.AddDate(0, 0, 1).After(cutoff) {
			continue
		}
		if _, err := m.pool.Exec(ctx, "DROP TABLE IF EXISTS "+pgx.Identifier{name}.Sanitize()); err != nil {
			return fmt.Errorf("drop partition %s: %w", name, err)
		}
		m.log.Info("dropped aged partition", "partition", name)
	}
	return nil
}

// Reconcile ensures future capacity and drops aged partitions in one pass.
func (m *Manager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureLocked(ctx); err != nil {
		return err
	}
	return m.dropAgedLocked(ctx)
}

// Repair ensures the partition covering ts exists. Called by the writer when
// an insert reports "no partition of relation found for row".
func (m *Manager) Repair(ctx context.Context, ts time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createLocked(ctx, ts)
}

// Run reconciles immediately and then on interval until ctx is cancelled.
func (m *Manager) Run(ctx context.Context, interval time.Duration) error {
	if err := m.Reconcile(ctx); err != nil {
		return err
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := m.Reconcile(ctx); err != nil {
				m.log.Error("partition reconcile failed", "err", err)
			}
		}
	}
}

func (m *Manager) createLocked(ctx context.Context, day time.Time) error {
	name := partitionName(day)
	var exists bool
	if err := m.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	start := day.UTC().Truncate(24 * time.Hour)
	end := start.AddDate(0, 0, 1)
	const layout = "2006-01-02 15:04:05-07:00"
	stmt := fmt.Sprintf(
		"CREATE TABLE %s PARTITION OF book_l1 FOR VALUES FROM ('%s') TO ('%s')",
		pgx.Identifier{name}.Sanitize(), start.Format(layout), end.Format(layout),
	)
	if _, err := m.pool.Exec(ctx, stmt); err != nil {
		// Another process may have created it between the check and the DDL.
		var stillMissing bool
		if qerr := m.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NULL", name).Scan(&stillMissing); qerr == nil && !stillMissing {
			return nil
		}
		return err
	}
	m.log.Info("created partition", "partition", name)
	return nil
}

func partitionName(day time.Time) string {
	return "book_l1_" + day.UTC().Format("20060102")
}

func todayUTC() time.Time {
	return time.Now().UTC().Truncate(24 * time.Hour)
}
