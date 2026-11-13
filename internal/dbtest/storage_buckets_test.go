package dbtest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The bucket migration is a no-op without Supabase's storage schema (plain
// Postgres) and, where storage.buckets exists, creates the buckets with
// their caps idempotently.
func TestStorageBucketsMigration(t *testing.T) {
	d := New(t)
	ctx := context.Background()
	src, err := os.ReadFile(filepath.Join(RepoRoot(), "supabase/migrations/20261113001100_storage_buckets.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `create schema storage;
		create table storage.buckets (id text primary key, name text not null, public boolean,
		  file_size_limit bigint, allowed_mime_types text[])`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // re-running must not fail
		if _, err := d.Pool.Exec(ctx, string(src)); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	want := map[string]struct {
		public bool
		limit  int64
	}{
		"avatars": {true, 3 << 20}, "community-media": {true, 5 << 20}, "post-media": {false, 50 << 20},
		"chat-media": {false, 25 << 20}, "resumes": {false, 10 << 20},
	}
	rows, err := d.Pool.Query(ctx, `select id, public, file_size_limit, 'application/pdf' = any(allowed_mime_types) from storage.buckets`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id string
		var pub, pdf bool
		var limit int64
		if err := rows.Scan(&id, &pub, &limit, &pdf); err != nil {
			t.Fatal(err)
		}
		n++
		w, ok := want[id]
		if !ok || w.public != pub || w.limit != limit {
			t.Errorf("bucket %s public=%v limit=%d", id, pub, limit)
		}
		if id == "community-media" && pdf {
			t.Error("community-media accepts PDFs")
		}
	}
	if n != len(want) {
		t.Errorf("buckets = %d, want %d", n, len(want))
	}
}
