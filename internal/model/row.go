// Package model holds the single L1 sample type shared by the pipeline.
package model

import "time"

// Row is one 1 Hz L1 sample. Prices and sizes are strings end-to-end and are
// cast to numeric only in SQL, so no floating point contamination is possible.
type Row struct {
	TS       time.Time // cycle boundary, UTC, second-truncated
	Symbol   string
	ExchTime time.Time // bookTicker "time" (transaction time)
	Bid      string
	BidQty   string
	Ask      string
	AskQty   string
}
