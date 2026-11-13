package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// addNotification inserts a notification directly (test setup) and returns its id.
func addNotification(t *testing.T, e *sessEnv, uid, kind string, created time.Time, read bool) string {
	t.Helper()
	var id string
	if err := e.td.Pool.QueryRow(context.Background(),
		`insert into public.notifications (user_id, kind, payload, created_at, read_at)
		 values ($1, $2, '{"ref":"x"}', $3, case when $4 then now() end) returning id`,
		uid, kind, created, read).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestListNotifications(t *testing.T) {
	e, _ := newMsgEnv(t)
	me, tok := admUser(t, e, "me@example.com")
	ann, aTok := admUser(t, e, "ann@example.com")

	// Signed out → 401.
	if code, _ := e.raw(t, "GET", "/me/notifications", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}

	// Empty inbox → 200 with [] (never null).
	req := httptest.NewRequest(http.MethodGet, "/me/notifications", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	if w.Code != 200 || w.Body.String() != "[]" || w.Header().Get("X-Unread-Count") != "0" {
		t.Fatalf("empty: %d %q %q", w.Code, w.Body, w.Header().Get("X-Unread-Count"))
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	oldest := addNotification(t, e, me, "login_alert", now.Add(-3*time.Hour), true)
	middle := addNotification(t, e, me, "job_interest", now.Add(-2*time.Hour), false)
	newest := addNotification(t, e, me, "mentorship", now.Add(-1*time.Hour), false)
	other := addNotification(t, e, ann, "mentorship", now, false)

	// Own rows only, newest first, Notification fields.
	_, out := e.raw(t, "GET", "/me/notifications", tok, nil)
	got := list(out)
	if len(got) != 3 || field(got[0], "id") != newest || field(got[1], "id") != middle || field(got[2], "id") != oldest {
		t.Fatalf("order: %v", got)
	}
	if hasID(out, other) {
		t.Fatalf("other user's notification leaked: %v", got)
	}
	n := got[0]
	for _, k := range []string{"id", "user_id", "kind", "payload", "read_at", "created_at"} {
		if _, ok := n.(map[string]any)[k]; !ok {
			t.Fatalf("missing %s: %v", k, n)
		}
	}
	if field(n, "user_id") != me || field(n, "kind") != "mentorship" || field(field(n, "payload"), "ref") != "x" || field(n, "read_at") != nil {
		t.Fatalf("row: %v", n)
	}
	if field(got[2], "read_at") == nil {
		t.Fatalf("read row has no read_at: %v", got[2])
	}

	// Unread filter and the badge header.
	req = httptest.NewRequest(http.MethodGet, "/me/notifications?unread=true", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	if w.Header().Get("X-Unread-Count") != "2" {
		t.Fatalf("unread header: %q", w.Header().Get("X-Unread-Count"))
	}
	_, out = e.raw(t, "GET", "/me/notifications?unread=true", tok, nil)
	if l := list(out); len(l) != 2 || hasID(out, oldest) {
		t.Fatalf("unread: %v", out)
	}

	// Paging: limit then before = last created_at.
	_, out = e.raw(t, "GET", "/me/notifications?limit=2", tok, nil)
	page := list(out)
	if len(page) != 2 || field(page[1], "id") != middle {
		t.Fatalf("page 1: %v", page)
	}
	_, out = e.raw(t, "GET", "/me/notifications?limit=2&before="+url.QueryEscape(field(page[1], "created_at").(string)), tok, nil)
	if l := list(out); len(l) != 1 || field(l[0], "id") != oldest {
		t.Fatalf("page 2: %v", out)
	}

	// Validation.
	for _, q := range []string{"limit=0", "limit=101", "limit=x", "before=yesterday", "unread=maybe"} {
		if code, _ := e.raw(t, "GET", "/me/notifications?"+q, tok, nil); code != 400 {
			t.Fatalf("%s: %d", q, code)
		}
	}

	// Another user sees only their own row.
	if _, out := e.raw(t, "GET", "/me/notifications", aTok, nil); len(list(out)) != 1 || !hasID(out, other) {
		t.Fatalf("ann: %v", out)
	}

	// End to end: a mentorship request shows in the recipient's bell.
	cid, _ := admCommunity(t, e, me, "Alumni", "private")
	chatJoin(t, e, cid, ann)
	code, mr := e.raw(t, "POST", "/users/"+ann+"/mentorship-requests", tok, map[string]any{"topic": "career", "message": "Advice?"})
	if code != 201 {
		t.Fatalf("mentorship: %d %v", code, mr)
	}
	_, out = e.raw(t, "GET", "/me/notifications", aTok, nil)
	l := list(out)
	if len(l) != 2 || field(l[0], "kind") != "mentorship" || field(field(l[0], "payload"), "mentorship_request_id") != field(mr, "id") {
		t.Fatalf("recipient inbox: %v", l)
	}
}
