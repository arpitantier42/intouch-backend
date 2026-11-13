package db_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/db"
	"github.com/intouch/api/internal/dbtest"
	"github.com/intouch/api/internal/rlsgate"
	"github.com/jackc/pgx/v5"
)

const secret = "integration-secret-integration-secret-1"

var ctx = context.Background()

func token(sub string) string {
	return auth.Sign(secret, map[string]any{"sub": sub, "role": "authenticated", "aud": "authenticated",
		"exp": time.Now().Add(time.Hour).Unix(), "session_id": "00000000-0000-0000-0000-000000000001"})
}

// harness wires a test database to an engine with the real middleware and
// a decision route shaped like the review-join-request endpoint: the RPC
// runs as the caller and its SQLSTATE becomes the HTTP status.
func harness(t *testing.T) (*dbtest.DB, *db.DB, *gin.Engine) {
	t.Helper()
	td := dbtest.New(t)
	d := &db.DB{Pool: td.Pool}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/communities/:id/join-requests/:userId/decision", auth.RequireUser(auth.NewVerifier(secret)), func(c *gin.Context) {
		var status string
		err := d.AsUser(c, auth.FromContext(c).Raw, func(tx pgx.Tx) error {
			return tx.QueryRow(c, `select status from public.review_join_request($1, $2, $3)`,
				c.Param("id"), c.Param("userId"), c.Query("decision")).Scan(&status)
		})
		if err != nil {
			apierr.Write(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": status})
	})
	return td, d, r
}

func community(t *testing.T, d *db.DB, owner, visibility string) string {
	t.Helper()
	var id string
	err := d.AsUser(ctx, []byte(`{"sub":"`+owner+`","role":"authenticated"}`), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select id::text from public.create_community('Alpha', 'online', gen_random_uuid(), null, $1)`, visibility).Scan(&id)
	})
	if err != nil {
		t.Fatalf("create community: %v", err)
	}
	return id
}

func claims(uid string) []byte { return []byte(`{"sub":"` + uid + `","role":"authenticated"}`) }

func TestMemberCannotApproveJoinRequest(t *testing.T) {
	td, d, r := harness(t)
	owner, member, applicant := td.NewUser(t, "Olive Owner"), td.NewUser(t, "Mona Member"), td.NewUser(t, "Abe Applicant")
	cid := community(t, d, owner, "public")
	if _, err := td.Pool.Exec(ctx, `insert into public.memberships (community_id, user_id, role, status, via)
		values ($1, $2, 'member', 'joined', 'direct')`, cid, member); err != nil {
		t.Fatal(err)
	}
	if err := d.AsUser(ctx, claims(applicant), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `select public.request_join($1)`, cid)
		return err
	}); err != nil {
		t.Fatalf("request join: %v", err)
	}

	call := func(uid string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/communities/"+cid+"/join-requests/"+applicant+"/decision?decision=approve", nil)
		if uid != "" {
			req.Header.Set("Authorization", "Bearer "+token(uid))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := call(""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s", w.Code, w.Body)
	}
	if w := call(member); w.Code != http.StatusForbidden {
		t.Fatalf("member approve: %d %s, want 403", w.Code, w.Body)
	}
	if w := call(applicant); w.Code != http.StatusForbidden {
		t.Fatalf("self approve: %d %s, want 403", w.Code, w.Body)
	}
	var status string
	_ = td.Pool.QueryRow(ctx, `select status from public.memberships where community_id = $1 and user_id = $2`, cid, applicant).Scan(&status)
	if status != "pending" {
		t.Fatalf("request should stay pending, got %q", status)
	}
	if w := call(owner); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "joined") {
		t.Fatalf("owner approve: %d %s", w.Code, w.Body)
	}
}

func TestAnonReadsZeroRowsFromMemberTables(t *testing.T) {
	td, d, _ := harness(t)
	owner := td.NewUser(t, "Olive Owner")
	community(t, d, owner, "public")
	community(t, d, owner, "private")

	var tables []string
	rows, err := td.Pool.Query(ctx, `select c.relname::text from pg_class c join pg_namespace n on n.oid = c.relnamespace
		join information_schema.role_table_grants g on g.table_schema = 'public' and g.table_name = c.relname
		where n.nspname = 'public' and c.relkind in ('r','v') and g.grantee = 'anon' and g.privilege_type = 'SELECT'
		group by 1 order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	tables, _ = pgx.CollectRows(rows, pgx.RowTo[string])
	if len(tables) == 0 {
		t.Fatal("expected anon-selectable tables to probe")
	}
	for _, tbl := range tables {
		var n int
		err := d.AsAnon(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select count(*) from public.`+pgx.Identifier{tbl}.Sanitize()).Scan(&n)
		})
		if err != nil && dbtest.SQLState(err) != "42501" {
			t.Fatalf("anon select %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("anon sees %d rows in %s", n, tbl)
		}
	}
}

func TestDataSurvivesANewSession(t *testing.T) {
	// Server-side state: what a user wrote is read back from Postgres with a
	// fresh session (as after clearing the app's local data).
	td, d, _ := harness(t)
	alice, bob := td.NewUser(t, "Alice A"), td.NewUser(t, "Bob B")
	cid := community(t, d, alice, "private")
	if err := d.AsUser(ctx, claims(alice), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `select public.send_connection_request($1)`, bob)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fresh := []byte(`{"sub":"` + alice + `","role":"authenticated","session_id":"00000000-0000-0000-0000-000000000002"}`)
	var communities, connections int
	if err := d.AsUser(ctx, fresh, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `select count(*) from public.communities where id = $1`, cid).Scan(&communities); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `select count(*) from public.connections`).Scan(&connections)
	}); err != nil {
		t.Fatal(err)
	}
	if communities != 1 || connections != 1 {
		t.Fatalf("after new session: communities=%d connections=%d", communities, connections)
	}
}

func TestRLSGate(t *testing.T) {
	td := dbtest.New(t)
	if err := rlsgate.Check(ctx, td.Pool); err != nil {
		t.Fatalf("migrated schema should pass: %v", err)
	}
	if _, err := td.Pool.Exec(ctx, `create table public.community_leak (id uuid primary key, community_id uuid)`); err != nil {
		t.Fatal(err)
	}
	err := rlsgate.Check(ctx, td.Pool)
	if err == nil || !strings.Contains(err.Error(), "community_leak") {
		t.Fatalf("gate should fail naming the table, got %v", err)
	}
	if _, err := td.Pool.Exec(ctx, `alter table public.community_leak enable row level security`); err != nil {
		t.Fatal(err)
	}
	if err := rlsgate.Check(ctx, td.Pool); err != nil {
		t.Fatalf("after enabling RLS: %v", err)
	}
}
