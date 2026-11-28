package spread

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	historyTimeout = 10 * time.Second
	liveTimeout    = 3 * time.Second
	liveLimit      = 500
)

// The range predicate is repeated on both legs so partition pruning is
// explicit on both sides of the join. Joining is on ts_sec, never exch_time:
// exch_time is a payload field with instrument-specific staleness.
const historySQL = `
WITH legs AS (
  SELECT a.ts_sec,
         a.ask AS ask_a,
         b.ask AS ask_b,
         a.ask / b.ask AS r
  FROM book_l1 a
  JOIN book_l1 b USING (ts_sec)
  WHERE a.symbol = $1 AND b.symbol = $2
    AND a.ts_sec >= $3 AND a.ts_sec < $4
    AND b.ts_sec >= $3 AND b.ts_sec < $4
    AND a.ask > 0 AND b.ask > 0
)
SELECT extract(epoch FROM date_bin($5::interval, ts_sec, to_timestamp(0)))::bigint AS t,
       count(*)::bigint AS n,
       avg(r)::double precision AS ratio,
       min(r)::double precision AS lo,
       max(r)::double precision AS hi,
       (array_agg(r ORDER BY ts_sec DESC))[1]::double precision AS last,
       avg(ask_a)::double precision AS ask_a,
       avg(ask_b)::double precision AS ask_b
FROM legs
GROUP BY 1
ORDER BY 1
`

// Extremes use the same cycle-aligned, both-leg range predicates as history.
// Both bid and ask must be positive; the ratio basis remains ask/ask.
const extremesLegsSQL = `
WITH legs AS (
  SELECT a.ts_sec,
         a.ask AS ask_a,
         b.ask AS ask_b,
         a.ask / b.ask AS r
  FROM book_l1 a
  JOIN book_l1 b USING (ts_sec)
  WHERE a.symbol = $1 AND b.symbol = $2
    AND a.ts_sec >= $3 AND a.ts_sec < $4
    AND b.ts_sec >= $3 AND b.ts_sec < $4
    AND a.bid > 0 AND a.ask > 0 AND b.bid > 0 AND b.ask > 0
)`

const extremesCountSQL = extremesLegsSQL + `
SELECT count(*) FROM legs
`

const extremesMinSQL = extremesLegsSQL + `
SELECT extract(epoch FROM ts_sec)::bigint AS t,
       r::double precision AS ratio,
       ask_a::double precision AS ask_a,
       ask_b::double precision AS ask_b
FROM legs
ORDER BY r ASC, ts_sec ASC
LIMIT 1
`

const extremesMaxSQL = extremesLegsSQL + `
SELECT extract(epoch FROM ts_sec)::bigint AS t,
       r::double precision AS ratio,
       ask_a::double precision AS ask_a,
       ask_b::double precision AS ask_b
FROM legs
ORDER BY r DESC, ts_sec ASC
LIMIT 1
`

const liveSelectSQL = `
SELECT extract(epoch FROM a.ts_sec)::bigint AS t,
       a.ask::double precision AS ask_a,
       b.ask::double precision AS ask_b,
       (a.ask / b.ask)::double precision AS ratio
FROM book_l1 a
JOIN book_l1 b USING (ts_sec)
WHERE a.symbol = $1 AND b.symbol = $2
`

// The lower bound on latest prevents a backward scan across all 45 daily
// partitions and protects the 3 s live timeout.
var liveSQL = liveSelectSQL + `
  AND a.ts_sec > to_timestamp($3) AND b.ts_sec > to_timestamp($3)
  AND a.ask > 0 AND b.ask > 0
ORDER BY a.ts_sec
LIMIT ` + strconv.Itoa(liveLimit) + `
`

const latestSQL = liveSelectSQL + `
  AND a.ts_sec > now() - interval '1 hour'
  AND b.ts_sec > now() - interval '1 hour'
  AND a.ask > 0 AND b.ask > 0
ORDER BY a.ts_sec DESC
LIMIT 1
`

// Store reads pair series from book_l1. It is intentionally read-only.
type Store struct {
	Pool *pgxpool.Pool
}

// NewStore wraps a pgx pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

// HistoryArgs builds the positional arguments for historySQL.
func HistoryArgs(pair Pair, from, to time.Time, bucketSQL string) []any {
	return []any{pair.Num, pair.Den, from, to, bucketSQL}
}

// LiveArgs builds the positional arguments for liveSQL. The cursor is passed
// as float64 because to_timestamp expects double precision.
func LiveArgs(pair Pair, after int64) []any {
	return []any{pair.Num, pair.Den, float64(after)}
}

// LatestArgs builds the positional arguments for latestSQL.
func LatestArgs(pair Pair) []any {
	return []any{pair.Num, pair.Den}
}

// scanner is satisfied by both pgx.Row and pgx.Rows, which keeps the row
// decoding logic testable without a database.
type scanner interface {
	Scan(dest ...any) error
}

// History returns downsampled buckets ordered by time.
func (s *Store) History(ctx context.Context, pair Pair, preset Preset, from, to time.Time) ([]Point, error) {
	ctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()

	rows, err := s.Pool.Query(ctx, historySQL, HistoryArgs(pair, from, to, preset.BucketSQL)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	points := make([]Point, 0)
	for rows.Next() {
		p, err := ScanHistoryRow(rows)
		if err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

// Live returns raw seconds strictly after the cursor, oldest first.
func (s *Store) Live(ctx context.Context, pair Pair, after int64) ([]LivePoint, error) {
	ctx, cancel := context.WithTimeout(ctx, liveTimeout)
	defer cancel()

	rows, err := s.Pool.Query(ctx, liveSQL, LiveArgs(pair, after)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	points := make([]LivePoint, 0)
	for rows.Next() {
		p, err := ScanLiveRow(rows)
		if err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

// Latest returns the most recent second in the last hour, or nil if none.
func (s *Store) Latest(ctx context.Context, pair Pair) (*Latest, error) {
	ctx, cancel := context.WithTimeout(ctx, liveTimeout)
	defer cancel()

	row := s.Pool.QueryRow(ctx, latestSQL, LatestArgs(pair)...)
	latest, err := ScanLatestRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &latest, nil
}

// Extremes returns the earliest-timestamp minimum and maximum ask/ask ratio
// in the window, plus the number of aligned points used.
func (s *Store) Extremes(ctx context.Context, pair Pair, preset Preset, from, to time.Time) (int64, *Point, *Point, error) {
	ctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()
	args := []any{pair.Num, pair.Den, from, to}

	var n int64
	if err := s.Pool.QueryRow(ctx, extremesCountSQL, args...).Scan(&n); err != nil {
		return 0, nil, nil, err
	}
	if n < 2 {
		return n, nil, nil, nil
	}
	minPoint, err := scanExtreme(ctx, s.Pool, extremesMinSQL, args)
	if err != nil {
		return n, nil, nil, err
	}
	maxPoint, err := scanExtreme(ctx, s.Pool, extremesMaxSQL, args)
	if err != nil {
		return n, nil, nil, err
	}
	return n, minPoint, maxPoint, nil
}

func scanExtreme(ctx context.Context, pool *pgxpool.Pool, sql string, args []any) (*Point, error) {
	var p Point
	if err := pool.QueryRow(ctx, sql, args...).Scan(&p.T, &p.Ratio, &p.AskA, &p.AskB); err != nil {
		return nil, err
	}
	p.N = 1
	return &p, nil
}

// ScanHistoryRow decodes one history row in the column order of historySQL.
func ScanHistoryRow(s scanner) (Point, error) {
	var p Point
	if err := s.Scan(&p.T, &p.N, &p.Ratio, &p.Lo, &p.Hi, &p.Last, &p.AskA, &p.AskB); err != nil {
		return Point{}, err
	}
	return p, nil
}

// ScanLiveRow decodes one raw second in the column order of liveSQL/latestSQL.
func ScanLiveRow(s scanner) (LivePoint, error) {
	var p LivePoint
	if err := s.Scan(&p.T, &p.AskA, &p.AskB, &p.Ratio); err != nil {
		return LivePoint{}, err
	}
	return p, nil
}

// ScanLatestRow decodes one raw second into the latest snapshot type.
func ScanLatestRow(s scanner) (Latest, error) {
	var p LivePoint
	if err := s.Scan(&p.T, &p.AskA, &p.AskB, &p.Ratio); err != nil {
		return Latest{}, err
	}
	return Latest{T: p.T, AskA: p.AskA, AskB: p.AskB, Ratio: p.Ratio}, nil
}
