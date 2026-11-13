package dbtest

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// newJob inserts a job as the poster (client insert under RLS).
func (d *DB) newJob(t *testing.T, poster, community, intent, title string) string {
	t.Helper()
	return scalar[string](t, d, poster, `insert into public.jobs (community_id, intent, title, apply_url)
		values ($1, $2, $3, 'https://example.com/apply') returning id::text`, community, intent, title)
}

// suInt runs a count query as the superuser.
func (d *DB) suInt(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

type jobItem struct {
	Kind       string
	ID         string
	Intent     string
	HasDetails bool
}

// jobsFeed lists jobs_feed for one community and intent as uid.
func (d *DB) jobsFeed(t *testing.T, uid, community, intent string) []jobItem {
	t.Helper()
	var items []jobItem
	err := d.As(uid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select item_kind, id::text, intent, has_details from public.jobs_feed
			where community_id = $1 and intent = $2 order by created_at desc, id desc`, community, intent)
		if err != nil {
			return err
		}
		items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (jobItem, error) {
			var it jobItem
			return it, r.Scan(&it.Kind, &it.ID, &it.Intent, &it.HasDetails)
		})
		return err
	})
	if err != nil {
		t.Fatalf("jobs_feed: %v", err)
	}
	return items
}

func feedHas(items []jobItem, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func TestJobsScopedPerCommunity(t *testing.T) {
	d, owner, member, outsider, cs := feedFixture(t)
	a, b := cs[0], cs[1]
	job := d.newJob(t, member, a, "opportunity", "Backend engineer")

	// Listed in A, not in B (switching scope), and the same list for both routes.
	inA := d.jobsFeed(t, member, a, "opportunity")
	if !feedHas(inA, job) || len(inA) != 1 || !inA[0].HasDetails || inA[0].Kind != "job" {
		t.Fatalf("A = %+v", inA)
	}
	if feedHas(d.jobsFeed(t, member, b, "opportunity"), job) {
		t.Fatal("job posted in A is listed in B")
	}
	if got := d.jobsFeed(t, owner, a, "opportunity"); len(got) != 1 || got[0].ID != job {
		t.Fatalf("owner A = %+v", got)
	}
	if feedHas(d.jobsFeed(t, member, a, "looking"), job) {
		t.Fatal("opportunity listed under looking")
	}

	// Non-members and anon see nothing; the base table too.
	if got := d.jobsFeed(t, outsider, a, "opportunity"); len(got) != 0 {
		t.Fatalf("outsider = %+v", got)
	}
	if got := d.jobsFeed(t, "", a, "opportunity"); len(got) != 0 {
		t.Fatalf("anon = %+v", got)
	}
	if n := scalar[int](t, d, outsider, `select count(*)::int from public.jobs where id = $1`, job); n != 0 {
		t.Fatal("outsider reads the job")
	}
	if !scalar[bool](t, d, member, `select public.can_see_job($1)`, job) ||
		scalar[bool](t, d, outsider, `select public.can_see_job($1)`, job) {
		t.Fatal("can_see_job")
	}

	// Only joined members post; title and apply link rules; server-set columns.
	wantCode(t, d.exec(outsider, `insert into public.jobs (community_id, intent, title) values ($1, 'looking', 'Role here')`, a), "PT403")
	wantCode(t, d.exec(member, `insert into public.jobs (community_id, intent, title) values ($1, 'looking', '   ')`, a), "23514")
	wantCode(t, d.exec(member, `insert into public.jobs (community_id, intent, title, apply_url) values ($1, 'looking', 'Role', 'ftp://x')`, a), "23514")
	wantCode(t, d.exec(member, `insert into public.jobs (community_id, intent, title, poster_id) values ($1, 'looking', 'Role', $2)`, a, owner), "42501")

	// Poster edits; others cannot; poster_id cannot move.
	if err := d.exec(member, `update public.jobs set title = 'Senior backend engineer' where id = $1`, job); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, d, owner, `with u as (update public.jobs set title = 'Hijack' where id = $1 returning 1) select count(*)::int from u`, job); n != 0 {
		t.Fatal("non-poster updated the job")
	}

	// Blocking hides the poster's jobs.
	d.su(t, `insert into public.blocks (blocker_id, blocked_id) values ($1, $2)`, owner, member)
	if feedHas(d.jobsFeed(t, owner, a, "opportunity"), job) {
		t.Fatal("blocked poster's job listed")
	}
	d.su(t, `delete from public.blocks`)

	// Delete: poster only; leaves the list and the detail page.
	wantCode(t, d.exec(owner, `select public.delete_job($1)`, job), "PT403")
	wantCode(t, d.exec(outsider, `select public.delete_job($1)`, job), "PT404")
	if err := d.exec(member, `select public.delete_job($1)`, job); err != nil {
		t.Fatal(err)
	}
	if feedHas(d.jobsFeed(t, member, a, "opportunity"), job) {
		t.Fatal("deleted job still listed")
	}
	if scalar[bool](t, d, member, `select public.can_see_job($1)`, job) {
		t.Fatal("deleted job visible")
	}
}

func TestJobTaggedPostsInJobs(t *testing.T) {
	d, _, member, _, cs := feedFixture(t)
	a, b := cs[0], cs[1]
	tag := func(p, tag string) {
		t.Helper()
		if err := d.exec(member, `update public.posts set job_tag = $2 where id = $1`, p, tag); err != nil {
			t.Fatal(err)
		}
	}
	avail := d.newPost(t, member, "text", "We are hiring a designer in Pune", a, b)
	tag(avail, "available")
	need := d.newPost(t, member, "text", "Looking for a frontend role, 3 yrs", a)
	tag(need, "need")
	general := d.newPost(t, member, "text", "Just a general update for all", a)

	opp := d.jobsFeed(t, member, a, "opportunity")
	if !feedHas(opp, avail) || feedHas(opp, need) || feedHas(opp, general) {
		t.Fatalf("opportunity = %+v", opp)
	}
	for _, it := range opp {
		if it.ID == avail && (it.Kind != "post" || it.HasDetails) {
			t.Fatalf("simple post must have no details page: %+v", it)
		}
	}
	// Posted to A and B: listed in both scopes.
	if !feedHas(d.jobsFeed(t, member, b, "opportunity"), avail) {
		t.Fatal("multi-community post missing from B")
	}
	look := d.jobsFeed(t, member, a, "looking")
	if !feedHas(look, need) || feedHas(look, general) {
		t.Fatalf("looking = %+v", look)
	}
	// A simple post has no job detail (getJob → 404).
	if scalar[bool](t, d, member, `select public.can_see_job($1)`, avail) {
		t.Fatal("post resolves as a job")
	}
	// Retag to general → leaves Jobs.
	tag(need, "general")
	if feedHas(d.jobsFeed(t, member, a, "looking"), need) {
		t.Fatal("retagged post still in Jobs")
	}
	// Delete from Home → leaves Opportunity.
	d.su(t, `update public.posts set deleted_at = now() where id = $1`, avail)
	if feedHas(d.jobsFeed(t, member, a, "opportunity"), avail) {
		t.Fatal("deleted post still in Jobs")
	}
}

func TestJobInterestsAndReplies(t *testing.T) {
	d, owner, member, outsider, cs := feedFixture(t)
	job := d.newJob(t, owner, cs[0], "opportunity", "Data analyst")

	submit := `select public.submit_job_interest($1, $2, $3, $4)::text`
	wantCode(t, d.exec(member, submit, job, "interest", strings.Repeat("x", 300), nil), "22001")
	wantCode(t, d.exec(member, submit, job, "interest", "Hi", "u/resume/a.jpg"), "23514")
	wantCode(t, d.exec(member, submit, job, "nope", "Hi", nil), "PT400")
	wantCode(t, d.exec(outsider, submit, job, "interest", "Hi", nil), "PT404")
	wantCode(t, d.exec(owner, submit, job, "interest", "Hi", nil), "PT400")
	wantCode(t, d.exec(member, `insert into public.job_interests (job_id, kind) values ($1, 'interest')`, job), "42501")

	scalar[string](t, d, member, submit, job, "interest", strings.Repeat("y", 280), "u/resume/cv.PDF")
	ref := scalar[string](t, d, member, `select (public.submit_job_interest($1, 'referral', 'Could you refer me?', 'u/resume/cv.docx')->>'id')`, job)
	wantCode(t, d.exec(member, submit, job, "interest", "again", nil), "23505")

	// Poster notified for each request.
	if n := d.suInt(t, `select count(*)::int from public.notifications where user_id = $1 and kind = 'job_interest'
		and payload->>'job_id' = $2::text`, owner, job); n != 2 {
		t.Fatalf("poster notifications = %d", n)
	}
	// Profile → Activity: the requester reads own requests; the poster reads them too; others not.
	if n := scalar[int](t, d, member, `select count(*)::int from public.job_interests where user_id = auth.uid()`); n != 2 {
		t.Fatalf("activity = %d", n)
	}
	if n := scalar[int](t, d, owner, `select count(*)::int from public.job_interests`); n != 2 {
		t.Fatalf("poster sees %d", n)
	}
	if n := scalar[int](t, d, outsider, `select count(*)::int from public.job_interests`); n != 0 {
		t.Fatalf("outsider sees %d", n)
	}

	// Reply: poster only; requester notified once.
	wantCode(t, d.exec(member, `select public.reply_job_interest($1)`, ref), "PT403")
	wantCode(t, d.exec(outsider, `select public.reply_job_interest($1)`, ref), "PT404")
	for i := 0; i < 2; i++ {
		if err := d.exec(owner, `select public.reply_job_interest($1)`, ref); err != nil {
			t.Fatal(err)
		}
	}
	if n := d.suInt(t, `select count(*)::int from public.notifications where user_id = $1 and kind = 'job_interest_reply'`, member); n != 1 {
		t.Fatalf("reply notifications = %d", n)
	}
	if !scalar[bool](t, d, member, `select replied_at is not null from public.job_interests where id = $1`, ref) {
		t.Fatal("replied_at not set")
	}
}

func TestJobBookmarks(t *testing.T) {
	d, owner, member, outsider, cs := feedFixture(t)
	job := d.newJob(t, owner, cs[0], "looking", "Open to PM roles")
	wantCode(t, d.exec(outsider, `insert into public.bookmarks (job_id) values ($1)`, job), "42501")
	if err := d.exec(member, `insert into public.bookmarks (job_id) values ($1)`, job); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, d, member, `select count(*)::int from public.bookmarks`); n != 1 {
		t.Fatalf("saved = %d", n)
	}
	if err := d.exec(owner, `select public.delete_job($1)`, job); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, d, member, `select count(*)::int from public.bookmarks`); n != 0 {
		t.Fatal("deleted job still in Saved")
	}
	wantCode(t, d.exec(member, `insert into public.bookmarks (job_id) values (gen_random_uuid())`), "42501")
}
