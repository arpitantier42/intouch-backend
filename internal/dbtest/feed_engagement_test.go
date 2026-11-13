package dbtest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// newPost inserts a post (standing in for the create-post RPC) and its
// targets as superuser; returns the post id.
func (d *DB) newPost(t *testing.T, author, kind, body string, communities ...string) string {
	t.Helper()
	id, err := d.postErr(author, kind, body, communities...)
	if err != nil {
		t.Fatalf("new post: %v", err)
	}
	return id
}

func (d *DB) postErr(author, kind, body string, communities ...string) (string, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var opts any
	if kind == "poll" {
		opts = `["Yes","No"]`
	}
	var id string
	if err := tx.QueryRow(ctx, `insert into public.posts (author_id, kind, body, poll_options, created_at)
		values ($1, $2, $3, $4::jsonb, clock_timestamp()) returning id::text`, author, kind, body, opts).Scan(&id); err != nil {
		return "", err
	}
	for _, c := range communities {
		if _, err := tx.Exec(ctx, `insert into public.post_targets (post_id, community_id) values ($1, $2)`, id, c); err != nil {
			return "", err
		}
	}
	return id, tx.Commit(ctx)
}

// feed returns the ids of one feed page as uid.
func (d *DB) feed(t *testing.T, uid, community string, args string, vals ...any) []string {
	t.Helper()
	var ids []string
	err := d.As(uid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select id::text from public.feed_page($1`+args+`)`, append([]any{community}, vals...)...)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	return ids
}

func contains(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// feedFixture: owner owns A, B, C; member is joined to all three;
// outsider is joined to none.
func feedFixture(t *testing.T) (d *DB, owner, member, outsider string, cs []string) {
	d = New(t)
	owner = d.NewUser(t, "Owner O")
	member = d.NewUser(t, "Member M")
	outsider = d.NewUser(t, "Outsider X")
	for _, n := range []string{"Alpha", "Beta", "Gamma"} {
		c := d.createCommunity(t, owner, n, "private")
		d.addMember(t, c, member, "member")
		cs = append(cs, c)
	}
	return
}

func TestFeedPaginationAndRefresh(t *testing.T) {
	d, owner, member, outsider, cs := feedFixture(t)
	var all []string
	for i := 0; i < 45; i++ {
		all = append(all, d.newPost(t, owner, "text", fmt.Sprintf("Post number %02d here", i), cs[0]))
	}
	// Infinite scroll: pages of 20 by keyset, no duplicates, newest first.
	seen := map[string]bool{}
	var cursorAt time.Time
	var cursorID string
	pages := 0
	for {
		var ids []string
		if pages == 0 {
			ids = d.feed(t, member, cs[0], "")
		} else {
			ids = d.feed(t, member, cs[0], ", 'newest', $2, $3::uuid", cursorAt, cursorID)
		}
		if len(ids) == 0 {
			break
		}
		if len(ids) > 20 {
			t.Fatalf("page size %d", len(ids))
		}
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("duplicate %s on page %d", id, pages)
			}
			seen[id] = true
		}
		last := ids[len(ids)-1]
		cursorID = last
		cursorAt = scalar[time.Time](t, d, member, `select created_at from public.posts where id = $1`, last)
		pages++
	}
	if len(seen) != 45 || pages != 3 {
		t.Fatalf("saw %d posts in %d pages", len(seen), pages)
	}
	first := d.feed(t, member, cs[0], "")
	if first[0] != all[44] {
		t.Fatalf("not newest first")
	}
	// Pull-to-refresh: only posts newer than the newest loaded.
	newest := scalar[time.Time](t, d, member, `select created_at from public.posts where id = $1`, first[0])
	fresh := d.newPost(t, owner, "text", "Freshly published post", cs[0])
	got := d.feed(t, member, cs[0], ", p_newer_than => $2", newest)
	if len(got) != 1 || got[0] != fresh {
		t.Fatalf("refresh = %v", got)
	}
	// Oldest sort.
	if ids := d.feed(t, member, cs[0], ", 'oldest'"); ids[0] != all[0] {
		t.Fatalf("oldest first wrong")
	}
	// Non-members: Members only; post rows invisible.
	err := d.exec(outsider, `select * from public.feed_page($1)`, cs[0])
	wantCode(t, err, "PT403")
	if n := scalar[int](t, d, outsider, `select count(*)::int from public.posts`); n != 0 {
		t.Fatalf("outsider sees %d posts", n)
	}
	if n := scalar[int](t, d, "", `select count(*)::int from public.posts`); n != 0 {
		t.Fatalf("anon sees %d posts", n)
	}
}

func TestMultiCommunityPostAndTargets(t *testing.T) {
	d, owner, member, outsider, cs := feedFixture(t)
	p := d.newPost(t, member, "text", "Hello to all my communities", cs...)
	for _, c := range cs {
		if !contains(d.feed(t, owner, c, ""), p) {
			t.Fatalf("post missing from %s", c)
		}
	}
	if n := scalar[int](t, d, member, `select count(*)::int from public.post_targets where post_id = $1`, p); n != 3 {
		t.Fatalf("targets = %d, want 3", n)
	}
	// Targeting a community the author has not joined is rejected.
	other := d.createCommunity(t, outsider, "Elsewhere", "private")
	_, err := d.postErr(member, "text", "Sneaking into another place", cs[0], other)
	wantCode(t, err, "PT403")
	// Content rules: too short text, single-option poll.
	_, err = d.postErr(member, "text", "short", cs[0])
	wantCode(t, err, "23514")
	_, err = d.Pool.Exec(ctx, `insert into public.posts (author_id, kind, body, poll_options)
		values ($1, 'poll', 'Which one do you prefer?', '["Only"]')`, member)
	wantCode(t, err, "23514")
	// Author edits stamp edited_at; others cannot edit.
	if err := d.exec(member, `update public.posts set body = 'Edited body text here' where id = $1`, p); err != nil {
		t.Fatal(err)
	}
	if !scalar[bool](t, d, member, `select edited_at is not null from public.posts where id = $1`, p) {
		t.Fatal("edited_at not set")
	}
	if n := scalar[int](t, d, owner, `with u as (update public.posts set body = 'Hijacked body text' where id = $1 returning 1) select count(*)::int from u`, p); n != 0 {
		t.Fatal("non-author edited post")
	}
}

func TestLikesCommentsAndPolls(t *testing.T) {
	d, owner, member, outsider, cs := feedFixture(t)
	p := d.newPost(t, owner, "text", "Please like this post", cs[0])
	like := `insert into public.likes (post_id) values ($1) on conflict (user_id, post_id) do nothing`
	for i := 0; i < 2; i++ {
		if err := d.exec(member, like, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.exec(owner, like, p); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, d, owner, `select like_count from public.posts where id = $1`, p); n != 2 {
		t.Fatalf("like_count = %d, want 2", n)
	}
	wantCode(t, d.exec(outsider, like, p), "42501")
	if err := d.exec(member, `delete from public.likes where post_id = $1`, p); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, d, owner, `select like_count from public.posts where id = $1`, p); n != 1 {
		t.Fatalf("like_count after unlike = %d", n)
	}

	// Comments: empty, add, reply nests, blank rejected, counters.
	if n := scalar[int](t, d, member, `select count(*)::int from public.comments where post_id = $1`, p); n != 0 {
		t.Fatal("expected no comments")
	}
	c1 := scalar[string](t, d, member, `insert into public.comments (post_id, body) values ($1, 'First!') returning id::text`, p)
	r1 := scalar[string](t, d, owner, `insert into public.comments (post_id, parent_id, body) values ($1, $2, 'Thanks') returning parent_id::text`, p, c1)
	if r1 != c1 {
		t.Fatal("reply not nested under parent")
	}
	wantCode(t, d.exec(member, `insert into public.comments (post_id, body) values ($1, '   ')`, p), "23514")
	if err := d.exec(owner, `insert into public.likes (comment_id) values ($1)`, c1); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, d, member, `select like_count from public.comments where id = $1`, c1); n != 1 {
		t.Fatalf("comment like_count = %d", n)
	}
	if n := scalar[int](t, d, member, `select comment_count from public.posts where id = $1`, p); n != 2 {
		t.Fatalf("comment_count = %d", n)
	}
	wantCode(t, d.exec(outsider, `insert into public.comments (post_id, body) values ($1, 'hi')`, p), "42501")

	// Polls: second vote replaces the first; out of range rejected.
	poll := d.newPost(t, owner, "poll", "Which option do you like?", cs[0])
	vote := `insert into public.poll_votes (post_id, option_index) values ($1, $2)
		on conflict (post_id, user_id) do update set option_index = excluded.option_index`
	for _, o := range []int{0, 1} {
		if err := d.exec(member, vote, poll, o); err != nil {
			t.Fatal(err)
		}
	}
	if s := scalar[string](t, d, member, `select public.poll_results($1)::text`, poll); s != "[0, 1]" {
		t.Fatalf("poll results %s", s)
	}
	wantCode(t, d.exec(member, vote, poll, 2), "PT400")
}

func TestBookmarksAndContentFilters(t *testing.T) {
	d, owner, member, _, cs := feedFixture(t)
	p1 := d.newPost(t, owner, "text", "Bookmark me please", cs[0])
	p2 := d.newPost(t, owner, "text", "Bookmark me as well", cs[0])
	for _, p := range []string{p1, p2} {
		if err := d.exec(member, `insert into public.bookmarks (post_id) values ($1) on conflict (user_id, post_id) do nothing`, p); err != nil {
			t.Fatal(err)
		}
	}
	saved := func() int { return scalar[int](t, d, member, `select count(*)::int from public.bookmarks`) }
	if saved() != 2 {
		t.Fatal("expected 2 saved")
	}
	d.su(t, `update public.posts set deleted_at = now() where id = $1`, p1)
	if saved() != 1 {
		t.Fatal("deleted post still in Saved")
	}
	if err := d.exec(member, `delete from public.bookmarks where post_id = $1`, p2); err != nil {
		t.Fatal(err)
	}
	if saved() != 0 {
		t.Fatal("un-bookmarked post still in Saved")
	}

	// Hide + Undo.
	p3 := d.newPost(t, owner, "text", "Something to hide away", cs[0])
	fid := scalar[string](t, d, member, `insert into public.content_filters (kind, post_id) values ('hide_post', $1) returning id::text`, p3)
	if contains(d.feed(t, member, cs[0], ""), p3) {
		t.Fatal("hidden post in feed")
	}
	if err := d.exec(member, `delete from public.content_filters where id = $1`, fid); err != nil {
		t.Fatal(err)
	}
	if !contains(d.feed(t, member, cs[0], ""), p3) {
		t.Fatal("post did not reappear after Undo")
	}
	// Mute author: their new posts stay out.
	if err := d.exec(member, `insert into public.content_filters (kind, muted_user_id) values ('mute_author', $1)`, owner); err != nil {
		t.Fatal(err)
	}
	p4 := d.newPost(t, owner, "text", "Posted after the mute", cs[0])
	if contains(d.feed(t, member, cs[0], ""), p4) {
		t.Fatal("muted author's post in feed")
	}
	// Blocks hide posts both ways.
	other := d.NewUser(t, "Other Y")
	d.addMember(t, cs[1], other, "member")
	p5 := d.newPost(t, other, "text", "From someone blocked", cs[1])
	if err := d.exec(other, `select public.block_user($1)`, member); err != nil {
		t.Fatal(err)
	}
	if contains(d.feed(t, member, cs[1], ""), p5) {
		t.Fatal("blocked user's post in feed")
	}
}

func TestReports(t *testing.T) {
	d, owner, member, _, cs := feedFixture(t)
	p := d.newPost(t, owner, "text", "Possibly a scam post", cs[0])
	wantCode(t, d.exec(member, `select public.create_report('post', $1, null)`, p), "PT400")
	raw := scalar[string](t, d, member, `select public.create_report('post', $1, 'fake_job', 'Looks fake')::text`, p)
	var r map[string]any
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	if r["status"] != "open" || r["reporter_id"] != nil {
		t.Fatalf("report = %v", r)
	}
	// The author (non-operator) sees nothing and cannot read reporter_id.
	if n := scalar[int](t, d, owner, `select count(*)::int from public.reports`); n != 0 {
		t.Fatalf("author sees %d reports", n)
	}
	wantCode(t, d.exec(owner, `select reporter_id from public.reports`), "42501")
	wantCode(t, d.exec(owner, `select public.list_reports()`), "PT403")
	// An operator retrieves it, still without reporter_id.
	op := d.NewUser(t, "Operator Z")
	d.su(t, `update public.profiles set is_operator = true where id = $1`, op)
	list := scalar[string](t, d, op, `select public.list_reports('open')::text`)
	if !strings.Contains(list, p) || strings.Contains(list, "reporter_id") {
		t.Fatalf("operator list = %s", list)
	}
	if n := scalar[int](t, d, op, `select count(*)::int from public.reports`); n != 1 {
		t.Fatalf("operator select = %d", n)
	}
}

// TestFeedPerformance seeds 25k members and 50k posts across communities and
// checks that a feed page is an index scan on post_targets_feed_idx and
// stays fast. EXPLAIN runs the same query feed_page() runs, as a member.
func TestFeedPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	d := New(t)
	owner := d.NewUser(t, "Owner O")
	big := d.createCommunity(t, owner, "Big", "public")
	small := d.createCommunity(t, owner, "Small", "public")
	start := time.Now()
	d.su(t, `
		with u as (insert into auth.users (id) select gen_random_uuid() from generate_series(1, 25000) returning id),
		p as (insert into public.profiles (id, full_name) select id, 'Member ' || left(id::text, 8) from u returning id)
		insert into public.memberships (community_id, user_id, role, status, via)
		select $1, id, 'member', 'joined', 'direct' from p`, big)
	d.su(t, `create table public.seed_authors as
		select user_id, row_number() over (order by user_id) rn from public.memberships
		where community_id = $1 and role = 'member' limit 1000`, big)
	d.su(t, `alter table public.seed_authors enable row level security`)
	d.su(t, `insert into public.memberships (community_id, user_id, role, status, via)
		select $1, user_id, 'member', 'joined', 'direct' from public.seed_authors`, small)
	d.su(t, `
		with authors as (select user_id, rn from public.seed_authors where $1::uuid is not null),
		posts as (
		  insert into public.posts (author_id, kind, body, created_at)
		  select a.user_id, 'text', 'Seeded post body number ' || g, now() - (g || ' seconds')::interval
		  from generate_series(1, 50000) g join authors a on a.rn = 1 + g % 1000
		  returning id, created_at)
		insert into public.post_targets (post_id, community_id, created_at)
		select id, case when random() < 0.9 then $1 else $2 end, created_at from posts`, big, small)
	// Some deleted posts, hides and mutes for the viewer.
	viewer := owner
	d.su(t, `update public.posts set deleted_at = now() where id in (select id from public.posts order by random() limit 500)`)
	d.su(t, `insert into public.content_filters (user_id, kind, post_id) select $1, 'hide_post', id from public.posts order by random() limit 200`, viewer)
	d.su(t, `analyze`)
	t.Logf("seeded in %s", time.Since(start).Round(time.Millisecond))

	query := `select p.* from public.post_targets t join public.posts p on p.id = t.post_id
		where t.community_id = $1 and p.deleted_at is null
		  and ($2::timestamptz is null or (t.created_at, t.post_id) < ($2::timestamptz, $3::uuid))
		  and not exists (select 1 from public.blocks b
		                  where (b.blocker_id = $4 and b.blocked_id = p.author_id)
		                     or (b.blocker_id = p.author_id and b.blocked_id = $4))
		  and not exists (select 1 from public.content_filters f
		                  where f.user_id = $4 and (f.post_id = p.id or f.muted_user_id = p.author_id))
		order by t.created_at desc, t.post_id desc limit 20`
	mid := scalar[time.Time](t, d, viewer, `select created_at from public.post_targets where community_id = $1 order by created_at desc offset 20000 limit 1`, big)
	for _, cur := range []any{nil, mid} {
		var plan []map[string]any
		err := d.As(viewer, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `explain (analyze, format json) `+query, big, cur, "ffffffff-ffff-ffff-ffff-ffffffffffff", viewer).Scan(&plan)
		})
		if err != nil {
			t.Fatal(err)
		}
		js, _ := json.Marshal(plan)
		text := string(js)
		ms := plan[0]["Execution Time"].(float64)
		t.Logf("cursor=%v execution %.2f ms", cur, ms)
		if !strings.Contains(text, "post_targets_feed_idx") {
			t.Fatalf("feed does not use post_targets_feed_idx: %s", text)
		}
		if rel := seqScanned(plan[0]["Plan"]); rel == "posts" || rel == "post_targets" {
			t.Fatalf("sequential scan on %s: %s", rel, text)
		}
		if ms > 50 {
			t.Fatalf("feed page took %.2f ms", ms)
		}
	}
	// The RPC itself, end to end.
	t0 := time.Now()
	ids := d.feed(t, viewer, big, "")
	if len(ids) != 20 {
		t.Fatalf("page = %d", len(ids))
	}
	if el := time.Since(t0); el > 200*time.Millisecond {
		t.Fatalf("feed_page took %s", el)
	}
	t.Logf("feed_page round trip %s", time.Since(t0).Round(time.Microsecond))
}

// seqScanned returns the first relation read by a Seq Scan in a JSON plan.
func seqScanned(node any) string {
	m, ok := node.(map[string]any)
	if !ok {
		return ""
	}
	if m["Node Type"] == "Seq Scan" {
		if rel, _ := m["Relation Name"].(string); rel == "posts" || rel == "post_targets" {
			return rel
		}
	}
	plans, _ := m["Plans"].([]any)
	for _, p := range plans {
		if r := seqScanned(p); r != "" {
			return r
		}
	}
	return ""
}
