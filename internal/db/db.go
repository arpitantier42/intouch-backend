// Package db runs queries with the caller's identity so Postgres RLS, not
// the Go code, decides what each request can read and write.
//
// Every request-scoped query goes through AsUser (role authenticated with
// the verified JWT claims), AsAnon (role anon) or AsService (role
// service_role, for the SECURITY DEFINER service functions). The pool's login
// role must be allowed to `set role` to each of them (Supabase's
// `authenticator`/`postgres` are).
package db

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps the connection pool.
type DB struct {
	Pool *pgxpool.Pool
}

// Open connects to url and checks the connection.
func Open(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

// Close closes the pool.
func (d *DB) Close() { d.Pool.Close() }

// AsUser runs fn in a transaction as role authenticated with claims (the
// raw verified JWT payload) as request.jwt.claims, so auth.uid() is the
// caller. The transaction commits when fn returns nil.
func (d *DB) AsUser(ctx context.Context, claims json.RawMessage, fn func(pgx.Tx) error) error {
	return d.as(ctx, "authenticated", string(claims), fn)
}

// AsAnon runs fn as role anon (unauthenticated endpoints).
func (d *DB) AsAnon(ctx context.Context, fn func(pgx.Tx) error) error {
	return d.as(ctx, "anon", `{"role":"anon"}`, fn)
}

// AsService runs fn as role service_role (auth wrappers, webhooks, scheduler).
func (d *DB) AsService(ctx context.Context, fn func(pgx.Tx) error) error {
	return d.as(ctx, "service_role", `{"role":"service_role"}`, fn)
}

func (d *DB) as(ctx context.Context, role, claims string, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select set_config('request.jwt.claims', $1, true)", claims); err != nil {
			return err
		}
		// role is one of three constants above, never user input.
		if _, err := tx.Exec(ctx, "set local role "+role); err != nil {
			return err
		}
		return fn(tx)
	})
}
