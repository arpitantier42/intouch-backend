// Package rlsgate is the deployment check that fails when any table in the
// public schema has row level security disabled.
package rlsgate

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Query lists ordinary and partitioned tables in public without RLS.
const Query = `select c.relname::text from pg_class c
join pg_namespace n on n.oid = c.relnamespace
where n.nspname = 'public' and c.relkind in ('r', 'p') and not c.relrowsecurity
order by 1`

// Querier is satisfied by *pgx.Conn, *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Check returns an error naming every public table without RLS.
func Check(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, Query)
	if err != nil {
		return fmt.Errorf("rls gate query: %w", err)
	}
	missing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("rls gate query: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("row level security is disabled on public tables: %s", strings.Join(missing, ", "))
	}
	return nil
}
