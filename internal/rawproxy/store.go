package rawproxy

import (
	"context"
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

// ScanRow decodes one raw row.
func ScanRow(s scanner) (Row, error) {
	var r Row
	err := s.Scan(&r.TS, &r.ExchTime, &r.Bid, &r.BidQty, &r.Ask, &r.AskQty)
	return r, err
}

// ScanSpreadRow decodes one book_l1_spread row.
func ScanSpreadRow(s scanner) (SpreadRow, error) {
	var r SpreadRow
	err := s.Scan(&r.TS, &r.Symbol, &r.Bid, &r.Ask, &r.BidQty, &r.AskQty, &r.Spread, &r.Mid)
	return r, err
}

// ScanSessionRow decodes one session_window row.
func ScanSessionRow(s scanner) (SessionRow, error) {
	var r SessionRow
	err := s.Scan(&r.Start, &r.End, &r.Kind)
	return r, err
}

// WindowStart returns the from bound of a query as a time.Time.
func (q Query) WindowStart() time.Time { return time.Unix(q.From, 0).UTC() }

// WindowEnd returns the to bound of a query as a time.Time.
func (q Query) WindowEnd() time.Time { return time.Unix(q.To, 0).UTC() }
