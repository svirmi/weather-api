package rawproxy

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const rowsDescSQL = `
SELECT extract(epoch FROM ts_sec)::bigint,
       extract(epoch FROM exch_time)::bigint,
       bid::double precision, bid_qty::double precision,
       ask::double precision, ask_qty::double precision
FROM book_l1
WHERE symbol = $1
  AND ts_sec >= to_timestamp($2) AND ts_sec < to_timestamp($3)
ORDER BY ts_sec DESC
LIMIT $4
`

const rowsAscSQL = `
SELECT extract(epoch FROM ts_sec)::bigint,
       extract(epoch FROM exch_time)::bigint,
       bid::double precision, bid_qty::double precision,
       ask::double precision, ask_qty::double precision
FROM book_l1
WHERE symbol = $1
  AND ts_sec >= to_timestamp($2) AND ts_sec < to_timestamp($3)
ORDER BY ts_sec ASC
LIMIT $4
`

const spreadDescSQL = `
SELECT extract(epoch FROM ts_sec)::bigint, symbol,
       bid::double precision, ask::double precision,
       bid_qty::double precision, ask_qty::double precision,
       spread::double precision, mid::double precision
FROM book_l1_spread
WHERE symbol = $1
  AND ts_sec >= to_timestamp($2) AND ts_sec < to_timestamp($3)
ORDER BY ts_sec DESC
LIMIT $4
`

const spreadAscSQL = `
SELECT extract(epoch FROM ts_sec)::bigint, symbol,
       bid::double precision, ask::double precision,
       bid_qty::double precision, ask_qty::double precision,
       spread::double precision, mid::double precision
FROM book_l1_spread
WHERE symbol = $1
  AND ts_sec >= to_timestamp($2) AND ts_sec < to_timestamp($3)
ORDER BY ts_sec ASC
LIMIT $4
`

const sessionsDescSQL = `
SELECT extract(epoch FROM start_time)::bigint,
       extract(epoch FROM end_time)::bigint, kind
FROM session_window
WHERE start_time >= to_timestamp($1) AND start_time < to_timestamp($2)
ORDER BY start_time DESC
LIMIT $3
`

const sessionsAscSQL = `
SELECT extract(epoch FROM start_time)::bigint,
       extract(epoch FROM end_time)::bigint, kind
FROM session_window
WHERE start_time >= to_timestamp($1) AND start_time < to_timestamp($2)
ORDER BY start_time ASC
LIMIT $3
`

const statsCountsSQL = `
SELECT symbol, count(*),
       min(extract(epoch FROM ts_sec)::bigint),
       max(extract(epoch FROM ts_sec)::bigint),
       count(*) FILTER (WHERE bid < ask),
       count(*) FILTER (WHERE bid >= ask)
FROM book_l1
WHERE ts_sec >= to_timestamp($1) AND ts_sec < to_timestamp($2)
GROUP BY symbol
`

const statsCountsOneSQL = `
SELECT symbol, count(*),
       min(extract(epoch FROM ts_sec)::bigint),
       max(extract(epoch FROM ts_sec)::bigint),
       count(*) FILTER (WHERE bid < ask),
       count(*) FILTER (WHERE bid >= ask)
FROM book_l1
WHERE ts_sec >= to_timestamp($1) AND ts_sec < to_timestamp($2)
  AND symbol = $3
GROUP BY symbol
`

const statsDupSQL = `
SELECT symbol, count(*) FROM (
  SELECT symbol FROM book_l1
  WHERE ts_sec >= to_timestamp($1) AND ts_sec < to_timestamp($2)
  GROUP BY ts_sec, symbol HAVING count(*) > 1
) d GROUP BY symbol
`

const statsDupOneSQL = `
SELECT symbol, count(*) FROM (
  SELECT symbol FROM book_l1
  WHERE ts_sec >= to_timestamp($1) AND ts_sec < to_timestamp($2)
    AND symbol = $3
  GROUP BY ts_sec, symbol HAVING count(*) > 1
) d GROUP BY symbol
`

const statsGapSQL = `
WITH g AS (
  SELECT symbol,
         extract(epoch FROM ts_sec)::bigint
           - lag(extract(epoch FROM ts_sec)::bigint) OVER (PARTITION BY symbol ORDER BY ts_sec) AS gap
  FROM book_l1
  WHERE ts_sec >= to_timestamp($1) AND ts_sec < to_timestamp($2)
)
SELECT symbol, max(gap) FROM g GROUP BY symbol
`

const statsGapOneSQL = `
WITH g AS (
  SELECT symbol,
         extract(epoch FROM ts_sec)::bigint
           - lag(extract(epoch FROM ts_sec)::bigint) OVER (PARTITION BY symbol ORDER BY ts_sec) AS gap
  FROM book_l1
  WHERE ts_sec >= to_timestamp($1) AND ts_sec < to_timestamp($2)
    AND symbol = $3
)
SELECT symbol, max(gap) FROM g GROUP BY symbol
`

const partitionSQL = `
SELECT c.relname FROM pg_inherits i
JOIN pg_class c ON c.oid = i.inhrelid
WHERE i.inhparent = 'book_l1'::regclass
  AND c.relname ~ '^book_l1_[0-9]{8}$'
`

// Store is the read-only database side of the proxy.
type Store struct {
	Pool *pgxpool.Pool
}

// NewStore wraps a pgx pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

// RowsArgs builds the positional arguments for the rows queries.
func RowsArgs(q Query) []any {
	return []any{q.Symbol, float64(q.From), float64(q.To), q.Limit}
}

// SessionsArgs builds the positional arguments for the session queries.
func SessionsArgs(q Query) []any {
	return []any{float64(q.From), float64(q.To), q.Limit}
}

func statsArgs(q Query) []any {
	args := []any{float64(q.From), float64(q.To)}
	if q.Symbol != "" {
		args = append(args, q.Symbol)
	}
	return args
}

type scanner interface {
	Scan(dest ...any) error
}

// Rows returns raw book_l1 observations.
func (s *Store) Rows(ctx context.Context, q Query) ([]Row, error) {
	ctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()
	sql := rowsDescSQL
	if q.Order == "asc" {
		sql = rowsAscSQL
	}
	rows, err := s.Pool.Query(ctx, sql, RowsArgs(q)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Row, 0)
	for rows.Next() {
		r, err := ScanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SpreadRows returns rows of the book_l1_spread view.
func (s *Store) SpreadRows(ctx context.Context, q Query) ([]SpreadRow, error) {
	ctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()
	sql := spreadDescSQL
	if q.Order == "asc" {
		sql = spreadAscSQL
	}
	rows, err := s.Pool.Query(ctx, sql, RowsArgs(q)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SpreadRow, 0)
	for rows.Next() {
		r, err := ScanSpreadRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Sessions returns session_window rows.
func (s *Store) Sessions(ctx context.Context, q Query) ([]SessionRow, error) {
	ctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()
	sql := sessionsDescSQL
	if q.Order == "asc" {
		sql = sessionsAscSQL
	}
	rows, err := s.Pool.Query(ctx, sql, SessionsArgs(q)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SessionRow, 0)
	for rows.Next() {
		r, err := ScanSessionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Stats returns a compact summary covering the DB-backed T9 checks.
func (s *Store) Stats(ctx context.Context, q Query) (Stats, error) {
	ctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()
	out := Stats{Symbols: make([]SymbolStats, 0, len(Symbols)), Partitions: []string{}, AgedPartitions: []string{}}
	bySymbol := make(map[string]*SymbolStats)
	for symbol := range Symbols {
		if q.Symbol != "" && q.Symbol != symbol {
			continue
		}
		st := SymbolStats{Symbol: symbol}
		out.Symbols = append(out.Symbols, st)
		bySymbol[symbol] = &out.Symbols[len(out.Symbols)-1]
	}

	countsSQL, dupSQL, gapSQL := statsCountsSQL, statsDupSQL, statsGapSQL
	if q.Symbol != "" {
		countsSQL, dupSQL, gapSQL = statsCountsOneSQL, statsDupOneSQL, statsGapOneSQL
	}
	args := statsArgs(q)

	rows, err := s.Pool.Query(ctx, countsSQL, args...)
	if err != nil {
		return Stats{}, err
	}
	for rows.Next() {
		var symbol string
		var n, first, last, good, bad int64
		if err := rows.Scan(&symbol, &n, &first, &last, &good, &bad); err != nil {
			rows.Close()
			return Stats{}, err
		}
		if st, ok := bySymbol[symbol]; ok {
			st.Rows = n
			st.FirstTS = first
			st.LastTS = last
			if total := good + bad; total > 0 {
				st.BidLTAskRatio = float64(good) / float64(total)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Stats{}, err
	}

	rows, err = s.Pool.Query(ctx, dupSQL, args...)
	if err != nil {
		return Stats{}, err
	}
	for rows.Next() {
		var symbol string
		var n int64
		if err := rows.Scan(&symbol, &n); err != nil {
			rows.Close()
			return Stats{}, err
		}
		if st, ok := bySymbol[symbol]; ok {
			st.Duplicates = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Stats{}, err
	}

	rows, err = s.Pool.Query(ctx, gapSQL, args...)
	if err != nil {
		return Stats{}, err
	}
	for rows.Next() {
		var symbol string
		var gap *int64
		if err := rows.Scan(&symbol, &gap); err != nil {
			rows.Close()
			return Stats{}, err
		}
		if gap != nil {
			if st, ok := bySymbol[symbol]; ok {
				st.MaxGapSec = *gap
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Stats{}, err
	}

	parts, err := s.Pool.Query(ctx, partitionSQL)
	if err != nil {
		return Stats{}, err
	}
	names := []string{}
	for parts.Next() {
		var name string
		if err := parts.Scan(&name); err != nil {
			parts.Close()
			return Stats{}, err
		}
		names = append(names, name)
	}
	parts.Close()
	if err := parts.Err(); err != nil {
		return Stats{}, err
	}
	out.Partitions = names

	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	cutoff := now.AddDate(0, 0, -RetentionDays).Truncate(24 * time.Hour)
	for _, name := range names {
		day, err := time.Parse("20060102", strings.TrimPrefix(name, "book_l1_"))
		if err != nil {
			continue
		}
		if day.After(today) {
			out.FuturePartitions++
		}
		if !day.AddDate(0, 0, 1).After(cutoff) {
			out.AgedPartitions = append(out.AgedPartitions, name)
		}
	}
	out.FutureOK = out.FuturePartitions >= PartitionAheadDays
	return out, nil
}

// ScanRow decodes one raw row.
func ScanRow(s scanner) (Row, error) {
	var r Row
	err := s.Scan(&r.TS, &r.ExchTime, &r.Bid, &r.BidQty, &r.Ask, &r.AskQty)
	return r, err
}

// ScanSpreadRow decodes one spread row.
func ScanSpreadRow(s scanner) (SpreadRow, error) {
	var r SpreadRow
	err := s.Scan(&r.TS, &r.Symbol, &r.Bid, &r.Ask, &r.BidQty, &r.AskQty, &r.Spread, &r.Mid)
	return r, err
}

// ScanSessionRow decodes one session row.
func ScanSessionRow(s scanner) (SessionRow, error) {
	var r SessionRow
	err := s.Scan(&r.Start, &r.End, &r.Kind)
	return r, err
}

// WindowStart returns the from bound of a query as a time.Time.
func (q Query) WindowStart() time.Time { return time.Unix(q.From, 0).UTC() }

// WindowEnd returns the to bound of a query as a time.Time.
func (q Query) WindowEnd() time.Time { return time.Unix(q.To, 0).UTC() }
