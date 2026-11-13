package server

import (
	"context"
	"strings"
	"testing"
)

// Clear messages keeping the peer's history and Message replacing Connect
// once connected are covered by TestChatThreads (threads_test.go) and
// TestMemberProfileMentorshipFollow (people_test.go); this file covers the
// report paths.

func TestReportsCreateAndOperatorList(t *testing.T) {
	e := newEngEnv(t)
	op, opTok := admUser(t, e.sessEnv, "operator@example.com")
	setProfile(t, e.sessEnv, op, `is_operator = true`)
	pid := postAs(t, e.sessEnv, e.ownerTok, e.cid, map[string]any{"body": "A post someone dislikes"})

	// No reason (or an unknown one) → 400 on the reason field; nothing stored.
	for name, body := range map[string]map[string]any{
		"missing": {"target_kind": "post", "target_id": pid},
		"blank":   {"target_kind": "post", "target_id": pid, "reason": ""},
		"unknown": {"target_kind": "post", "target_id": pid, "reason": "boring"},
	} {
		code, out := e.raw(t, "POST", "/reports", e.memTok, body)
		if code != 400 || field(field(field(out, "error"), "fields"), "reason") != "Choose a reason." {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	for name, body := range map[string]map[string]any{
		"kind":    {"target_kind": "event", "target_id": pid, "reason": "spam"},
		"id":      {"target_kind": "post", "target_id": "nope", "reason": "spam"},
		"details": {"target_kind": "post", "target_id": pid, "reason": "spam", "details": strings.Repeat("d", 1001)},
	} {
		if code, out := e.raw(t, "POST", "/reports", e.memTok, body); code != 400 {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, _ := e.raw(t, "POST", "/reports", "", map[string]any{"target_kind": "post", "target_id": pid, "reason": "spam"}); code != 401 {
		t.Fatalf("anonymous report: %d", code)
	}

	// Post report: 201, status open, reporter_id never returned (even when sent).
	code, out := e.raw(t, "POST", "/reports", e.memTok, map[string]any{
		"target_kind": "post", "target_id": pid, "reason": "inappropriate",
		"details": "  rude  ", "reporter_id": e.owner,
	})
	if code != 201 || field(out, "status") != "open" || field(out, "details") != "rude" || field(out, "target_id") != pid {
		t.Fatalf("report post: %d %v", code, out)
	}
	if _, has := out.(map[string]any)["reporter_id"]; has {
		t.Fatalf("reporter_id returned: %v", out)
	}
	postReport := field(out, "id").(string)
	var reporter, status string
	if err := e.td.Pool.QueryRow(context.Background(),
		`select reporter_id::text, status from public.reports where id = $1`, postReport).Scan(&reporter, &status); err != nil {
		t.Fatal(err)
	}
	if reporter != e.mem || status != "open" {
		t.Fatalf("stored reporter %s status %s", reporter, status)
	}

	// Report conversation → a thread report.
	code, th := e.raw(t, "POST", "/communities/"+e.cid+"/threads/direct?user_id="+e.owner, e.memTok, nil)
	if code != 200 {
		t.Fatalf("open dm: %d %v", code, th)
	}
	tid := field(th, "id").(string)
	if code, out := e.raw(t, "POST", "/reports", e.memTok, map[string]any{
		"target_kind": "thread", "target_id": tid, "reason": "harassment"}); code != 201 || field(out, "target_kind") != "thread" {
		t.Fatalf("report thread: %d %v", code, out)
	}

	// Fake job / scam reason against a job.
	var jid string
	if err := e.td.Pool.QueryRow(context.Background(),
		`insert into public.jobs (community_id, poster_id, intent, title) values ($1, $2, 'opportunity', 'Pay to apply') returning id::text`,
		e.cid, e.owner).Scan(&jid); err != nil {
		t.Fatal(err)
	}
	if code, out := e.raw(t, "POST", "/reports", e.memTok, map[string]any{
		"target_kind": "job", "target_id": jid, "reason": "fake_job"}); code != 201 || field(out, "reason") != "fake_job" {
		t.Fatalf("report job: %d %v", code, out)
	}

	// Report from a member profile → a user report.
	if code, out := e.raw(t, "POST", "/reports", e.memTok, map[string]any{
		"target_kind": "user", "target_id": e.owner, "reason": "other"}); code != 201 || field(out, "target_id") != e.owner {
		t.Fatalf("report user: %d %v", code, out)
	}

	// Non-operators (the reporter, and the post's author) → 403.
	for name, tok := range map[string]string{"reporter": e.memTok, "author": e.ownerTok, "outsider": e.outTok} {
		if code, out := e.raw(t, "GET", "/admin/reports", tok, nil); code != 403 || field(field(out, "error"), "message") != "Operators only" {
			t.Errorf("%s list: %d %v", name, code, out)
		}
	}
	// The author cannot read the table directly either, and reporter_id is
	// not selectable even for operators.
	if code, out := e.raw(t, "GET", "/admin/reports?status=open", e.ownerTok, nil); code != 403 {
		t.Fatalf("author filtered list: %d %v", code, out)
	}

	// Operator: every report, newest first, no reporter_id anywhere.
	code, all := e.raw(t, "GET", "/admin/reports", opTok, nil)
	if code != 200 || len(list(all)) != 4 {
		t.Fatalf("operator list: %d %v", code, all)
	}
	kinds := map[string]bool{}
	for _, r := range list(all) {
		if _, has := r.(map[string]any)["reporter_id"]; has {
			t.Fatalf("reporter_id in listing: %v", r)
		}
		kinds[field(r, "target_kind").(string)] = true
	}
	if !kinds["post"] || !kinds["thread"] || !kinds["job"] || !kinds["user"] || field(list(all)[3], "id") != postReport {
		t.Fatalf("listing: %v", all)
	}

	// Status filter.
	if _, err := e.td.Pool.Exec(context.Background(), `update public.reports set status = 'dismissed' where id = $1`, postReport); err != nil {
		t.Fatal(err)
	}
	if code, out := e.raw(t, "GET", "/admin/reports?status=open", opTok, nil); code != 200 || len(list(out)) != 3 || hasID(out, postReport) {
		t.Fatalf("open filter: %d %v", code, out)
	}
	if code, out := e.raw(t, "GET", "/admin/reports?status=dismissed", opTok, nil); code != 200 || !hasID(out, postReport) || len(list(out)) != 1 {
		t.Fatalf("dismissed filter: %d %v", code, out)
	}
	if code, _ := e.raw(t, "GET", "/admin/reports?status=bogus", opTok, nil); code != 400 {
		t.Fatalf("bad status: %d", code)
	}
}
