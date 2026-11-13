package uploads

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The storage buckets must enforce exactly the caps and types the API checks.
func TestBucketMigrationMatchesPolicy(t *testing.T) {
	src, err := os.ReadFile("../../supabase/migrations/20261113001100_storage_buckets.sql")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile(`(?s)\('([a-z-]+)', '[a-z-]+', (?:true|false), (\d+),\s*array\[(.*?)\]\)`)
	quoted := regexp.MustCompile(`'([^']+)'`)
	got := map[string]bool{}
	for _, m := range row.FindAllStringSubmatch(string(src), -1) {
		bucket := m[1]
		got[bucket] = true
		want, ok := Buckets()[bucket]
		if !ok {
			t.Errorf("bucket %s is not used by any purpose", bucket)
			continue
		}
		if n, _ := strconv.ParseInt(m[2], 10, 64); n != want.MaxBytes {
			t.Errorf("%s: file_size_limit %d, policy %d", bucket, n, want.MaxBytes)
		}
		var types []string
		for _, q := range quoted.FindAllStringSubmatch(m[3], -1) {
			types = append(types, q[1])
		}
		var wantTypes []string
		for k := range want.MIME {
			wantTypes = append(wantTypes, k)
		}
		sort.Strings(types)
		sort.Strings(wantTypes)
		if strings.Join(types, ",") != strings.Join(wantTypes, ",") {
			t.Errorf("%s: mime types\n got %v\nwant %v", bucket, types, wantTypes)
		}
	}
	for b := range Buckets() {
		if !got[b] {
			t.Errorf("bucket %s missing from the migration", b)
		}
	}
}
