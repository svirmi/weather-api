// Package migrate applies the idempotent base schema.
package migrate

import (
	"context"
	_ "embed"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// Run applies the schema. PostgreSQL supports multiple statements in an Exec
// without parameters via the simple query protocol, which pgx uses here.
func Run(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schema)
	return err
}
