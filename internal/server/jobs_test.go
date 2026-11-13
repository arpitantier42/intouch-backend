package server

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func createJobAs(t *testing.T, e *rsvpEnv, tok, cid string, body map[string]any) string {
	t.Helper()
	code, out := e.raw(t, "POST", "/communities/"+cid+"/jobs", tok, body)
	if code != 201 {
		t.Fatalf("create job: %d %v", code, out)
	}
	return field(out, "id").(string)
}

func jobIDs(t *testing.T, e *rsvpEnv, tok, path string) []string {
	t.Helper()
	return feedIDs(t, e.sessEnv, tok, path)
}

// backdateJob moves a job's created_at (the update trigger keeps it fixed).
func backdateJob(t *testing.T, e *rsvpEnv, id string, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, e.td.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `set local session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `update public.jobs set created_at = now() - $2::interval where id = $1`,
			id, age.String())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func fieldMsg(out any, name string) any {
	errObj, _ := field(out, "error").(map[string]any)
	fields, _ := errObj["fields"].(map[string]any)
	return fields[name]
}

func TestJobsCreateListScopeSearchSort(t *testing.T) {
	e := newRsvpEnv(t)
	owner, oTok := admUser(t, e.sessEnv, "owner@example.com")
	a, _ := admCommunity(t, e.sessEnv, owner, "Alpha", "private")
	b, _ := admCommunity(t, e.sessEnv, owner, "Beta", "private")
	mem, mTok := rsvpUser(t, e, a, "asha@example.com")
	_, outTok := admUser(t, e.sessEnv, "out@example.com")
	if _, err := e.td.Pool.Exec(context.Background(),
		`update public.profiles set full_name = 'Asha Kulkarni' where id = $1`, mem); err != nil {
		t.Fatal(err)
	}

	// Validation: nothing is published.
	code, out := e.raw(t, "POST", "/communities/"+a+"/jobs", oTok, map[string]any{"title": "  ", "company": "Acme"})
	if code != 400 || fieldMsg(out, "title") != jobTitleMsg || field(field(out, "error"), "message") != jobTitleMsg {
		t.Fatalf("no title: %d %v", code, out)
	}
	code, out = e.raw(t, "POST", "/communities/"+a+"/jobs", oTok, map[string]any{"title": "SRE", "apply_url": "ftp://x.example.com"})
	if code != 400 || fieldMsg(out, "apply_url") != jobApplyURLMsg {
		t.Fatalf("ftp link: %d %v", code, out)
	}
	code, out = e.raw(t, "POST", "/communities/"+a+"/jobs", oTok, map[string]any{"title": "SRE", "job_type": "gig"})
	if code != 400 || fieldMsg(out, "job_type") == nil {
		t.Fatalf("bad type: %d %v", code, out)
	}
	var n int
	_ = e.td.Pool.QueryRow(context.Background(), `select count(*) from public.jobs`).Scan(&n)
	if n != 0 {
		t.Fatalf("rejected posts created %d jobs", n)
	}
	// Not joined → 403.
	if code, out := e.raw(t, "POST", "/communities/"+a+"/jobs", outTok, map[string]any{"title": "SRE"}); code != 403 {
		t.Fatalf("outsider create: %d %v", code, out)
	}

	code, out = e.raw(t, "POST", "/communities/"+a+"/jobs", oTok, map[string]any{
		"title": "Backend engineer", "company": "Acme Corp", "location": "Pune", "job_type": "Full-time",
		"work_mode": "Remote", "apply_url": "https://acme.example.com/jobs/1", "details": "Go and Postgres."})
	if code != 201 || field(out, "job_type") != "full_time" || field(out, "work_mode") != "remote" ||
		field(out, "intent") != "opportunity" || field(out, "has_details") != true || field(out, "mine") != true {
		t.Fatalf("create: %d %v", code, out)
	}
	acme := field(out, "id").(string)
	old := createJobAs(t, e, oTok, a, map[string]any{"title": "Data analyst", "company": "Globex"})
	backdateJob(t, e, old, 8*24*time.Hour)
	looking := createJobAs(t, e, mTok, a, map[string]any{"intent": "looking", "title": "Product designer", "location": "Delhi"})
	inB := createJobAs(t, e, oTok, b, map[string]any{"title": "Teacher"})

	// Opportunity is the default; Looking hides hiring posts.
	opp := jobIDs(t, e, mTok, "/communities/"+a+"/jobs")
	if len(opp) != 2 || opp[0] != acme || opp[1] != old {
		t.Fatalf("opportunity newest: %v", opp)
	}
	if ids := jobIDs(t, e, mTok, "/communities/"+a+"/jobs?intent=looking"); len(ids) != 1 || ids[0] != looking {
		t.Fatalf("looking: %v", ids)
	}
	// Scope: B's role is not in A; the member of A only cannot read B.
	if contains(opp, inB) {
		t.Fatal("B's job listed in A")
	}
	if code, _ := e.raw(t, "GET", "/communities/"+b+"/jobs", mTok, nil); code != 403 {
		t.Fatalf("not joined list: %d", code)
	}
	// Main and community routes are the same query: the owner sees the same roles.
	if ids := jobIDs(t, e, oTok, "/communities/"+a+"/jobs?intent=opportunity"); len(ids) != 2 || ids[0] != acme {
		t.Fatalf("same list: %v", ids)
	}
	// Search by company, empty result, people search on Looking.
	if ids := jobIDs(t, e, mTok, "/communities/"+a+"/jobs?q=acme"); len(ids) != 1 || ids[0] != acme {
		t.Fatalf("company search: %v", ids)
	}
	if ids := jobIDs(t, e, mTok, "/communities/"+a+"/jobs?q=nothing-matches"); len(ids) != 0 {
		t.Fatalf("empty search: %v", ids)
	}
	if ids := jobIDs(t, e, oTok, "/communities/"+a+"/jobs?intent=looking&q=kulkarni"); len(ids) != 1 {
		t.Fatalf("person search: %v", ids)
	}
	// Past week hides the 8-day-old role; Oldest puts it first.
	if ids := jobIDs(t, e, mTok, "/communities/"+a+"/jobs?since=week"); len(ids) != 1 || ids[0] != acme {
		t.Fatalf("since week: %v", ids)
	}
	if ids := jobIDs(t, e, mTok, "/communities/"+a+"/jobs?sort=oldest"); len(ids) != 2 || ids[0] != old {
		t.Fatalf("oldest: %v", ids)
	}
	if code, out := e.raw(t, "GET", "/communities/"+a+"/jobs?since=year", mTok, nil); code != 400 {
		t.Fatalf("bad since: %d %v", code, out)
	}
}

func TestJobsTaggedPosts(t *testing.T) {
	e := newRsvpEnv(t)
	owner, oTok := admUser(t, e.sessEnv, "owner@example.com")
	a, _ := admCommunity(t, e.sessEnv, owner, "Alpha", "private")
	_, mTok := rsvpUser(t, e, a, "asha@example.com")

	avail := postAs(t, e.sessEnv, oTok, a, map[string]any{"body": "We are hiring a Go developer in Pune", "job_tag": "available"})
	need := postAs(t, e.sessEnv, oTok, a, map[string]any{"body": "Looking for a frontend role, 3 yrs React", "job_tag": "need"})
	general := postAs(t, e.sessEnv, oTok, a, map[string]any{"body": "Hello everyone, welcome to Alpha!"})

	code, out := e.raw(t, "GET", "/communities/"+a+"/jobs", mTok, nil)
	items, _ := out.([]any)
	if code != 200 || len(items) != 1 || field(items[0], "id") != avail ||
		field(items[0], "item_kind") != "post" || field(items[0], "has_details") != false {
		t.Fatalf("opportunity with tagged post: %d %v", code, out)
	}
	looking := jobIDs(t, e, mTok, "/communities/"+a+"/jobs?intent=looking")
	if len(looking) != 1 || looking[0] != need || contains(looking, general) {
		t.Fatalf("looking: %v", looking)
	}
	// No details page for a tagged post.
	if code, _ := e.raw(t, "GET", "/jobs/"+avail, mTok, nil); code != 404 {
		t.Fatalf("post as job detail: %d", code)
	}
	// Retag to general → leaves Jobs; delete → leaves Jobs.
	if code, out := e.raw(t, "PATCH", "/posts/"+need, oTok, map[string]any{"job_tag": "general"}); code != 200 {
		t.Fatalf("retag: %d %v", code, out)
	}
	if ids := jobIDs(t, e, mTok, "/communities/"+a+"/jobs?intent=looking"); len(ids) != 0 {
		t.Fatalf("retagged post still listed: %v", ids)
	}
	if code, _ := e.raw(t, "DELETE", "/posts/"+avail, oTok, nil); code != 204 {
		t.Fatalf("delete post: %d", code)
	}
	if ids := jobIDs(t, e, mTok, "/communities/"+a+"/jobs"); len(ids) != 0 {
		t.Fatalf("deleted post still listed: %v", ids)
	}
}

func TestJobDetailEditDelete(t *testing.T) {
	e := newRsvpEnv(t)
	owner, oTok := admUser(t, e.sessEnv, "owner@example.com")
	a, _ := admCommunity(t, e.sessEnv, owner, "Alpha", "private")
	mem, mTok := rsvpUser(t, e, a, "asha@example.com")
	_, outTok := admUser(t, e.sessEnv, "out@example.com")
	id := createJobAs(t, e, oTok, a, map[string]any{"title": "SRE", "company": "Acme", "salary": "₹30L"})

	code, out := e.raw(t, "GET", "/jobs/"+id, mTok, nil)
	if code != 200 || field(out, "title") != "SRE" || field(out, "poster_name") == nil ||
		field(out, "can_message") != true || field(out, "mine") != false {
		t.Fatalf("detail: %d %v", code, out)
	}
	for _, path := range []string{"/jobs/" + id, "/jobs/00000000-0000-0000-0000-000000000000", "/jobs/nope"} {
		tok := mTok
		if path == "/jobs/"+id {
			tok = outTok
		}
		if code, _ := e.raw(t, "GET", path, tok, nil); code != 404 {
			t.Errorf("%s: %d", path, code)
		}
	}
	// Non-poster edit/delete → 403; poster edit ok.
	if code, _ := e.raw(t, "PATCH", "/jobs/"+id, mTok, map[string]any{"title": "Hacked"}); code != 403 {
		t.Fatalf("non-poster edit: %d", code)
	}
	if code, _ := e.raw(t, "DELETE", "/jobs/"+id, mTok, nil); code != 403 {
		t.Fatalf("non-poster delete: %d", code)
	}
	code, out = e.raw(t, "PATCH", "/jobs/"+id, oTok, map[string]any{"title": "Senior SRE", "salary": nil,
		"community_id": "00000000-0000-0000-0000-000000000000", "poster_id": mem})
	if code != 200 || field(out, "title") != "Senior SRE" || field(out, "salary") != nil || field(out, "community_id") != a {
		t.Fatalf("poster edit: %d %v", code, out)
	}
	if code, out := e.raw(t, "PATCH", "/jobs/"+id, oTok, map[string]any{"title": ""}); code != 400 || fieldMsg(out, "title") != jobTitleMsg {
		t.Fatalf("blank title edit: %d %v", code, out)
	}
	if code, _ := e.raw(t, "PATCH", "/jobs/"+id, oTok, map[string]any{"apply_url": "javascript:alert(1)"}); code != 400 {
		t.Fatalf("bad apply link edit: %d", code)
	}
	// Delete → leaves lists and detail.
	if code, _ := e.raw(t, "DELETE", "/jobs/"+id, oTok, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if ids := jobIDs(t, e, mTok, "/communities/"+a+"/jobs"); len(ids) != 0 {
		t.Fatalf("deleted job listed: %v", ids)
	}
	if code, _ := e.raw(t, "GET", "/jobs/"+id, oTok, nil); code != 404 {
		t.Fatalf("deleted detail: %d", code)
	}
}

func TestJobInterestsNotifyAndPush(t *testing.T) {
	e := newRsvpEnv(t)
	owner, oTok := admUser(t, e.sessEnv, "owner@example.com")
	a, _ := admCommunity(t, e.sessEnv, owner, "Alpha", "private")
	mem, mTok := rsvpUser(t, e, a, "asha@example.com")
	id := createJobAs(t, e, oTok, a, map[string]any{"title": "SRE", "company": "Acme"})
	path := "/jobs/" + id + "/interests"

	long := make([]rune, 300)
	for i := range long {
		long[i] = 'a'
	}
	if code, out := e.raw(t, "POST", path, mTok, map[string]any{"note": string(long)}); code != 400 || fieldMsg(out, "note") == nil {
		t.Fatalf("300-char note: %d %v", code, out)
	}
	if code, out := e.raw(t, "POST", path, mTok, map[string]any{"resume_url": "u/resume/cv.jpg"}); code != 415 {
		t.Fatalf("jpg resume: %d %v", code, out)
	}
	if code, _ := e.raw(t, "POST", path, oTok, map[string]any{}); code != 400 {
		t.Fatalf("own job: %d", code)
	}
	code, out := e.raw(t, "POST", path, mTok, map[string]any{"kind": "referral", "note": "Worked on SRE at Globex.",
		"resume_url": "u/resume/cv.pdf"})
	if code != 201 || field(out, "kind") != "referral" || field(out, "resume_url") != "u/resume/cv.pdf" {
		t.Fatalf("referral: %d %v", code, out)
	}
	interestID := field(out, "id").(string)
	if code, _ := e.raw(t, "POST", path, mTok, map[string]any{"kind": "referral"}); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	if code, _ := e.raw(t, "POST", path, mTok, nil); code != 201 { // default kind interest
		t.Fatalf("interest: %d", code)
	}

	// Poster: stored notification + push.
	p := e.push.last(t, 2)
	if len(p.UserIDs) != 1 || p.UserIDs[0] != owner || p.Data["job_id"] != id {
		t.Fatalf("push: %+v", p)
	}
	var n int
	_ = e.td.Pool.QueryRow(context.Background(),
		`select count(*) from public.notifications where user_id = $1 and kind = 'job_interest'`, owner).Scan(&n)
	if n != 2 {
		t.Fatalf("poster notifications: %d", n)
	}
	// Requester sees the request (Profile → Activity) and it shows on the detail.
	var mine int
	if err := e.td.As(mem, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `select count(*) from public.job_interests`).Scan(&mine)
	}); err != nil || mine != 2 {
		t.Fatalf("activity: %d %v", mine, err)
	}
	if _, out := e.raw(t, "GET", "/jobs/"+id, mTok, nil); len(field(out, "my_interests").([]any)) != 2 {
		t.Fatalf("my_interests: %v", out)
	}
	// Poster replies → requester notified.
	if _, err := e.rpcAs(t, owner, `select public.reply_job_interest($1)::text`, interestID); err != nil {
		t.Fatal(err)
	}
	_ = e.td.Pool.QueryRow(context.Background(),
		`select count(*) from public.notifications where user_id = $1 and kind = 'job_interest_reply'`, mem).Scan(&n)
	if n != 1 {
		t.Fatalf("reply notification: %d", n)
	}
}

func TestLookingCandidateBlockHidesCard(t *testing.T) {
	e := newRsvpEnv(t)
	owner, oTok := admUser(t, e.sessEnv, "owner@example.com")
	a, _ := admCommunity(t, e.sessEnv, owner, "Alpha", "private")
	cand, cTok := rsvpUser(t, e, a, "cand@example.com")
	id := createJobAs(t, e, cTok, a, map[string]any{"intent": "looking", "title": "Designer"})

	code, out := e.raw(t, "GET", "/communities/"+a+"/jobs?intent=looking", oTok, nil)
	items, _ := out.([]any)
	if code != 200 || len(items) != 1 || field(items[0], "can_message") != true || field(items[0], "poster_id") != cand {
		t.Fatalf("looking card: %d %v", code, out)
	}
	// Message candidate opens the DM in this community.
	if code, out := e.raw(t, "POST", "/communities/"+a+"/threads/direct?user_id="+cand, oTok, nil); code != 200 ||
		field(out, "community_id") != a {
		t.Fatalf("message candidate: %d %v", code, out)
	}
	// The candidate blocks the viewer: the card (and Message) is gone.
	if _, err := e.td.Pool.Exec(context.Background(),
		`insert into public.blocks (blocker_id, blocked_id) values ($1, $2)`, cand, owner); err != nil {
		t.Fatal(err)
	}
	if ids := jobIDs(t, e, oTok, "/communities/"+a+"/jobs?intent=looking"); len(ids) != 0 {
		t.Fatalf("blocked candidate still listed: %v", ids)
	}
	if code, _ := e.raw(t, "GET", "/jobs/"+id, oTok, nil); code != 404 {
		t.Fatalf("blocked detail: %d", code)
	}
}
