package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
)

// admUser creates a user with a profile and returns (id, token).
func admUser(t *testing.T, e *sessEnv, email string) (string, string) {
	t.Helper()
	id, tok := meUser(t, e, email)
	if w, _ := e.do(t, http.MethodGet, "/me", tok, "", nil); w.Code != 200 {
		t.Fatalf("profile for %s: %d", email, w.Code)
	}
	return id, tok
}

// admCommunity creates a community owned by uid with one required question.
func admCommunity(t *testing.T, e *sessEnv, uid, name, visibility string) (cid, qid string) {
	t.Helper()
	err := e.td.As(uid, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`select id from public.create_community($1, 'online', gen_random_uuid(), null, $2)`, name, visibility).Scan(&cid)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.td.Pool.QueryRow(context.Background(),
		`insert into public.community_questions (community_id, label, kind, required) values ($1, 'Why join?', 'short_text', true) returning id`,
		cid).Scan(&qid); err != nil {
		t.Fatal(err)
	}
	return cid, qid
}

// raw performs a request and returns the status and the decoded JSON body.
func (e *sessEnv) raw(t *testing.T, method, path, token string, body any) (int, any) {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	var out any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func field(v any, k string) any {
	m, _ := v.(map[string]any)
	return m[k]
}

func TestAdmissionFlow(t *testing.T) {
	e := newSessEnv(t)
	owner, ownerTok := admUser(t, e, "owner@example.com")
	admin, adminTok := admUser(t, e, "admin@example.com")
	req, reqTok := admUser(t, e, "req@example.com")
	_, otherTok := admUser(t, e, "other@example.com")

	cid, qid := admCommunity(t, e, owner, "Gophers", "public")
	priv, _ := admCommunity(t, e, owner, "Secret", "private")
	base := "/communities/" + cid

	// The admin joins by request and is approved by the owner, then promoted.
	if code, out := e.raw(t, "POST", base+"/join-requests", adminTok, []map[string]string{{"question_id": qid, "value": "hi"}}); code != 201 {
		t.Fatalf("admin request: %d %v", code, out)
	}
	if code, out := e.raw(t, "POST", base+"/join-requests/"+admin+"/decision?decision=approve", ownerTok, nil); code != 200 {
		t.Fatalf("approve admin: %d %v", code, out)
	}
	if code, out := e.raw(t, "PATCH", base+"/members/"+admin, ownerTok, map[string]string{"role": "superadmin"}); code != 400 {
		t.Fatalf("superadmin role: %d %v", code, out)
	}
	if code, out := e.raw(t, "PATCH", base+"/members/"+admin, ownerTok, map[string]string{"role": "admin"}); code != 200 || field(out, "role") != "admin" {
		t.Fatalf("promote: %d %v", code, out)
	}

	// Request: blank required answer, private community, success, duplicate.
	if code, _ := e.raw(t, "POST", base+"/join-requests", reqTok, map[string]any{"answers": []map[string]string{{"question_id": qid, "value": " "}}}); code != 400 {
		t.Fatalf("blank answer: %d", code)
	}
	if code, _ := e.raw(t, "POST", "/communities/"+priv+"/join-requests", reqTok, nil); code != 403 {
		t.Fatalf("private: %d", code)
	}
	code, out := e.raw(t, "POST", base+"/join-requests", reqTok, map[string]string{"question_id": qid, "value": "Love Go"})
	if code != 201 || field(out, "status") != "pending" || field(out, "via") != "request" {
		t.Fatalf("request: %d %v", code, out)
	}
	if code, _ := e.raw(t, "POST", base+"/join-requests", reqTok, nil); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}

	// Roster: member and pending users get 403 'Admins only' and no rows.
	code, out = e.raw(t, "GET", base+"/members", reqTok, nil)
	if code != 403 || field(field(out, "error"), "message") != "Admins only" {
		t.Fatalf("pending roster: %d %v", code, out)
	}
	code, out = e.raw(t, "GET", base+"/members?status=pending", adminTok, nil)
	list, _ := out.([]any)
	if code != 200 || len(list) != 1 || field(list[0], "user_id") != req {
		t.Fatalf("requests tab: %d %v", code, out)
	}
	answers, _ := field(list[0], "answers").([]any)
	if len(answers) != 1 || field(answers[0], "value") != "Love Go" {
		t.Fatalf("answers: %v", list[0])
	}

	// Review: self-review rejected and stays pending; unknown 404; approve.
	if code, _ := e.raw(t, "POST", base+"/join-requests/"+req+"/decision?decision=approve", reqTok, nil); code != 403 {
		t.Fatalf("self review: %d", code)
	}
	if code, _ := e.raw(t, "POST", base+"/join-requests/"+owner+"/decision?decision=approve", adminTok, nil); code != 404 {
		t.Fatalf("no pending: %d", code)
	}
	code, out = e.raw(t, "POST", base+"/join-requests/"+req+"/decision?decision=approve", adminTok, nil)
	if code != 200 || field(out, "status") != "joined" || field(out, "via") != "request" || field(out, "reviewed_by") != admin {
		t.Fatalf("approve: %d %v", code, out)
	}

	// Roles: admin cannot promote; members tab lists role labels.
	if code, _ := e.raw(t, "PATCH", base+"/members/"+req, adminTok, map[string]string{"role": "admin"}); code != 403 {
		t.Fatalf("admin promotes: %d", code)
	}
	if code, out := e.raw(t, "PATCH", base+"/members/"+req, ownerTok, map[string]string{"role": "moderator"}); code != 200 || field(out, "role") != "moderator" {
		t.Fatalf("owner -> moderator: %d %v", code, out)
	}
	_, out = e.raw(t, "GET", base+"/members", adminTok, nil)
	list, _ = out.([]any)
	if len(list) != 3 || field(list[0], "role_label") != "Owner" || field(list[2], "role_label") != "Moderator" {
		t.Fatalf("members tab: %v", out)
	}
	if code, _ := e.raw(t, "GET", base+"/members", otherTok, nil); code != 403 {
		t.Fatalf("non-member roster: %d", code)
	}

	// Removal: self and owner rejected; member removed → access ends.
	if code, _ := e.raw(t, "DELETE", base+"/members/"+admin, adminTok, nil); code != 403 {
		t.Fatalf("remove self: %d", code)
	}
	if code, _ := e.raw(t, "DELETE", base+"/members/"+owner, adminTok, nil); code != 403 {
		t.Fatalf("remove owner: %d", code)
	}
	if code, _ := e.raw(t, "DELETE", base+"/members/"+req, adminTok, nil); code != 204 {
		t.Fatalf("remove: %d", code)
	}
	if code, _ := e.raw(t, "DELETE", "/me/memberships/"+cid, reqTok, nil); code != 404 {
		t.Fatalf("removed user has no membership: %d", code)
	}

	// Leave: owner blocked until transfer; transfer demotes to admin.
	if code, _ := e.raw(t, "DELETE", "/me/memberships/"+cid, ownerTok, nil); code != 409 {
		t.Fatalf("owner leave: %d", code)
	}
	if code, _ := e.raw(t, "POST", base+"/transfer?to_user_id="+admin, adminTok, nil); code != 403 {
		t.Fatalf("non-owner transfer: %d", code)
	}
	if code, _ := e.raw(t, "POST", base+"/transfer?to_user_id="+req, ownerTok, nil); code != 422 {
		t.Fatalf("transfer to non-member: %d", code)
	}
	code, out = e.raw(t, "POST", base+"/transfer?to_user_id="+admin, ownerTok, nil)
	if code != 200 || field(out, "role") != "owner" || field(out, "user_id") != admin {
		t.Fatalf("transfer: %d %v", code, out)
	}
	var prev string
	_ = e.td.Pool.QueryRow(context.Background(),
		`select role from public.memberships where community_id = $1 and user_id = $2`, cid, owner).Scan(&prev)
	if prev != "admin" {
		t.Fatalf("previous owner role: %q", prev)
	}
	if code, _ := e.raw(t, "DELETE", "/me/memberships/"+cid, ownerTok, nil); code != 204 {
		t.Fatalf("leave after transfer: %d", code)
	}

	// Withdraw a pending request.
	if code, _ := e.raw(t, "POST", base+"/join-requests", otherTok, map[string]string{"question_id": qid, "value": "x"}); code != 201 {
		t.Fatalf("other request: %d", code)
	}
	if code, _ := e.raw(t, "DELETE", "/me/memberships/"+cid, otherTok, nil); code != 204 {
		t.Fatalf("withdraw: %d", code)
	}
	if code, _ := e.raw(t, "GET", base+"/members?status=bogus", adminTok, nil); code != 400 {
		t.Fatalf("bad status: %d", code)
	}
	if code, _ := e.raw(t, "GET", base+"/members", "", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
}
