package dbtest

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var createPgtap = regexp.MustCompile(`(?im)^create extension if not exists pgtap.*$`)

// TestPgTAPFiles runs every supabase/tests/database/*.test.sql file. With the
// pgtap extension unavailable it uses db/test/pgtap_shim.sql instead; the real
// suite runs with `supabase test db`.
func TestPgTAPFiles(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(RepoRoot(), "supabase", "tests", "database", "*.test.sql"))
	if len(files) == 0 {
		t.Skip("no pgTAP files")
	}
	shim, err := os.ReadFile(filepath.Join(RepoRoot(), "db", "test", "pgtap_shim.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			d := New(t)
			var available bool
			_ = d.Pool.QueryRow(ctx, `select exists (select 1 from pg_available_extensions where name = 'pgtap')`).Scan(&available)
			sql, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			body := string(sql)
			if !available {
				if _, err := d.Pool.Exec(ctx, string(shim)); err != nil {
					t.Fatalf("shim: %v", err)
				}
				body = createPgtap.ReplaceAllString(body, "")
			}
			conn, err := d.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, body); err != nil {
				_, _ = conn.Exec(ctx, "rollback")
				t.Fatalf("%v", err)
			}
		})
	}
}
