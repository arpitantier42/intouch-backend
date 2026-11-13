// Package dbtest runs schema and policy tests against a real Postgres.
//
// Set TEST_DATABASE_URL to a superuser connection string (for example
// postgres://postgres@localhost:55432/postgres). Each test gets a fresh
// database with db/test/supabase_stub.sql and every file in
// supabase/migrations applied in name order. Without the variable the tests
// are skipped.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is a throwaway database with the migrations applied.
type DB struct {
	Pool *pgxpool.Pool // superuser connection: bypasses RLS
}

// RepoRoot returns the repository root directory.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// New creates a fresh migrated database, dropped when the test ends.
func New(t *testing.T) *DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx)

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "intouch_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), url)
		if err == nil {
			_, _ = c.Exec(context.Background(), "drop database if exists "+name+" with (force)")
			c.Close(context.Background())
		}
	})

	root := RepoRoot()
	files := []string{filepath.Join(root, "db", "test", "supabase_stub.sql")}
	migrations, _ := filepath.Glob(filepath.Join(root, "supabase", "migrations", "*.sql"))
	sort.Strings(migrations)
	files = append(files, migrations...)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	return &DB{Pool: pool}
}

// NewUser inserts an auth user and its profile and returns the id.
func (d *DB) NewUser(t *testing.T, fullName string) string {
	t.Helper()
	var id string
	err := d.Pool.QueryRow(context.Background(), `
		with u as (insert into auth.users default values returning id)
		insert into public.profiles (id, full_name) select id, $1 from u returning id::text`,
		fullName).Scan(&id)
	if err != nil {
		t.Fatalf("new user: %v", err)
	}
	return id
}

// As runs fn in a transaction as the API role "authenticated" with uid as
// the JWT subject (uid "" runs as "anon"). It commits when fn returns nil.
func (d *DB) As(uid string, fn func(tx pgx.Tx) error) error {
	ctx := context.Background()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	role := "authenticated"
	if uid == "" {
		role = "anon"
	}
	claims := fmt.Sprintf(`{"sub":%q,"role":%q}`, uid, role)
	if uid == "" {
		claims = `{"role":"anon"}`
	}
	if _, err := tx.Exec(ctx, "select set_config('request.jwt.claims', $1, true)", claims); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "set local role "+role); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SQLState returns the Postgres error code of err, or "".
func SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
