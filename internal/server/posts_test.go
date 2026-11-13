package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// postAs publishes a post through the API and returns its id.
func postAs(t *testing.T, e *sessEnv, tok, ids string, body map[string]any) string {
	t.Helper()
	path := "/posts"
	if ids != "" {
		path += "?community_ids=" + ids
	}
	code, out := e.raw(t, "POST", path, tok, body)
	if code != 201 {
		t.Fatalf("create post: %d %v", code, out)
	}
	return field(out, "id").(string)
}

func feedIDs(t *testing.T, e *sessEnv, tok, path string) []string {
	t.Helper()
	code, out := e.raw(t, "GET", path, tok, nil)
	if code != 200 {
		t.Fatalf("feed %s: %d %v", path, code, out)
	}
	var ids []string
	for _, p := range out.([]any) {
		ids = append(ids, field(p, "id").(string))
	}
	return ids
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func setRole(t *testing.T, e *sessEnv, cid, uid, role string) {
	t.Helper()
	if _, err := e.td.Pool.Exec(context.Background(),
		`update public.memberships set role = $3 where community_id = $1 and user_id = $2`, cid, uid, role); err != nil {
		t.Fatal(err)
	}
}

func TestPostCreateAndScope(t *testing.T) {
	e := newSessEnv(t)
	owner, ownerTok := admUser(t, e, "owner@example.com")
	mem, memTok := admUser(t, e, "member@example.com")
	_, outTok := admUser(t, e, "outsider@example.com")
	a, _ := admCommunity(t, e, owner, "Alpha", "public")
	b, _ := admCommunity(t, e, owner, "Beta", "public")
	c, _ := admCommunity(t, e, owner, "Gamma", "public")
	chatJoin(t, e, a, mem)

	// All communities (no community_ids) → every joined one, target_count 3.
	code, out := e.raw(t, "POST", "/posts", ownerTok, map[string]any{"body": "Hello to all three communities"})
	if code != 201 || field(out, "target_count") != float64(3) || field(out, "kind") != "text" {
		t.Fatalf("all: %d %v", code, out)
	}
	all := field(out, "id").(string)
	for _, cid := range []string{a, b, c} {
		if !contains(feedIDs(t, e, ownerTok, "/communities/"+cid+"/posts"), all) {
			t.Fatalf("post missing from %s", cid)
		}
	}

	// A post only to B is not in A's feed.
	onlyB := postAs(t, e, ownerTok, b, map[string]any{"body": "Only for the Beta folks"})
	if contains(feedIDs(t, e, ownerTok, "/communities/"+a+"/posts"), onlyB) {
		t.Fatal("B post leaked into A")
	}

	// Validation: 9 chars, > 1200, no content, two media, one-option poll, empty targets.
	for name, tc := range map[string]struct {
		path string
		body map[string]any
		want int
	}{
		"short":       {"/posts", map[string]any{"body": "123456789"}, 400},
		"long":        {"/posts", map[string]any{"body": strings.Repeat("x", 1201)}, 400},
		"empty":       {"/posts", map[string]any{}, 400},
		"two media":   {"/posts", map[string]any{"body": "Two photos at once", "media": []map[string]any{{"url": "a.png", "kind": "image"}, {"url": "b.png", "kind": "image"}}}, 400},
		"one option":  {"/posts", map[string]any{"body": "Pick one of these", "poll_options": []string{"only"}}, 400},
		"no target":   {"/posts?community_ids=", map[string]any{"body": "Nowhere to post this"}, 400},
		"big image":   {"/posts", map[string]any{"body": "A very big photo", "media_url": "x.png", "media_kind": "image", "media_bytes": 6 << 20}, 413},
		"big video":   {"/posts", map[string]any{"body": "A very big video", "media_url": "x.mp4", "media_kind": "video", "media_bytes": 51 << 20}, 413},
		"not joined":  {"/posts?community_ids=" + b, map[string]any{"body": "Sneaking into Beta"}, 403},
		"member annc": {"/posts?community_ids=" + a, map[string]any{"body": "Important announcement", "is_announcement": true}, 403},
	} {
		if code, out := e.raw(t, "POST", tc.path, memTok, tc.body); code != tc.want {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	// Exactly 10 and 1200 characters are fine; a poll with two options too.
	postAs(t, e, memTok, a, map[string]any{"body": "1234567890"})
	postAs(t, e, memTok, a, map[string]any{"body": strings.Repeat("y", 1200)})
	code, out = e.raw(t, "POST", "/posts?community_ids="+a, memTok,
		map[string]any{"body": "Which day suits you", "poll_options": []string{"Sat", "Sun"}})
	if code != 201 || field(out, "kind") != "poll" {
		t.Fatalf("poll: %d %v", code, out)
	}
	// Owner may post an announcement.
	postAs(t, e, ownerTok, a, map[string]any{"body": "Official announcement here", "is_announcement": true})

	// Non-members: feed 403 'Members only'; shared link 404.
	code, out = e.raw(t, "GET", "/communities/"+a+"/posts", outTok, nil)
	if code != 403 || !strings.Contains(fmt.Sprint(out), "Members only") {
		t.Fatalf("outsider feed: %d %v", code, out)
	}
	if code, _ := e.raw(t, "GET", "/posts/"+all, outTok, nil); code != 404 {
		t.Fatalf("outsider get: %d", code)
	}
	if code, out := e.raw(t, "GET", "/posts/"+all, memTok, nil); code != 200 || field(out, "id") != all {
		t.Fatalf("member get: %d %v", code, out)
	}
	if code, out := e.raw(t, "GET", "/posts/00000000-0000-0000-0000-000000000000", memTok, nil); code != 404 ||
		!strings.Contains(fmt.Sprint(out), "Post not found") {
		t.Fatalf("unknown: %d %v", code, out)
	}
}

func TestFeedFiltersAndPaging(t *testing.T) {
	e := newSessEnv(t)
	owner, tok := admUser(t, e, "owner@example.com")
	cid, _ := admCommunity(t, e, owner, "Alpha", "public")
	base := "/communities/" + cid + "/posts"

	plain := postAs(t, e, tok, cid, map[string]any{"body": "Just a plain text post"})
	urgent := postAs(t, e, tok, cid, map[string]any{"body": "Urgent: need a ride", "is_urgent": true})
	annc := postAs(t, e, tok, cid, map[string]any{"body": "Announcement for everyone", "is_announcement": true})
	job := postAs(t, e, tok, cid, map[string]any{"body": "Hiring a Go developer", "job_tag": "available"})
	mentor := postAs(t, e, tok, cid, map[string]any{"body": "Looking for a mentor in design"})
	old := postAs(t, e, tok, cid, map[string]any{"body": "Something from last week"})
	// created_at is immutable by trigger; backdate with triggers off.
	if _, err := e.td.Pool.Exec(context.Background(), fmt.Sprintf(`
		begin;
		set local session_replication_role = replica;
		update public.posts set created_at = now() - interval '3 days' where id = '%[1]s';
		update public.post_targets set created_at = now() - interval '3 days' where post_id = '%[1]s';
		commit;`, old)); err != nil {
		t.Fatal(err)
	}
	// An event post (inserted as the event RPC would).
	var ev string
	if err := e.td.Pool.QueryRow(context.Background(), `
		with e as (insert into public.events (community_id, creator_id, title, starts_at, ends_at, format, venue)
		           values ($1, $2, 'Meetup', now() + interval '1 day', now() + interval '1 day 2 hours', 'in_person', 'Hall')
		           returning id),
		     p as (insert into public.posts (author_id, kind, body, event_id) select $2, 'event', 'Meetup', id from e returning id)
		select id from p`, cid, owner).Scan(&ev); err != nil {
		t.Fatal(err)
	}
	if _, err := e.td.Pool.Exec(context.Background(),
		`insert into public.post_targets (post_id, community_id) values ($1, $2)`, ev, cid); err != nil {
		t.Fatal(err)
	}

	ids := feedIDs(t, e, tok, base)
	if ids[0] != ev || ids[len(ids)-1] != old {
		t.Fatalf("newest first: %v", ids)
	}
	if o := feedIDs(t, e, tok, base+"?sort=oldest"); o[0] != old {
		t.Fatalf("oldest first: %v", o)
	}
	u := feedIDs(t, e, tok, base+"?chip=urgent")
	if !contains(u, urgent) || !contains(u, annc) || contains(u, plain) {
		t.Fatalf("urgent chip: %v", u)
	}
	an := feedIDs(t, e, tok, base+"?chip=announcements")
	if !contains(an, annc) || contains(an, urgent) {
		t.Fatalf("announcements chip: %v", an)
	}
	if j := feedIDs(t, e, tok, base+"?type=jobs"); len(j) != 1 || j[0] != job {
		t.Fatalf("jobs: %v", j)
	}
	if m := feedIDs(t, e, tok, base+"?type=mentorship"); len(m) != 1 || m[0] != mentor {
		t.Fatalf("mentorship: %v", m)
	}
	if ev2 := feedIDs(t, e, tok, base+"?type=events"); len(ev2) != 1 || ev2[0] != ev {
		t.Fatalf("events: %v", ev2)
	}
	if s := feedIDs(t, e, tok, base+"?since=24h"); contains(s, old) || len(s) != 6 {
		t.Fatalf("since 24h: %v", s)
	}
	if code, _ := e.raw(t, "GET", base+"?chip=bogus", tok, nil); code != 400 {
		t.Fatalf("bad chip: %d", code)
	}

	// Paging: 45 posts → 20 + 20 + rest with no duplicates.
	for i := 0; i < 38; i++ {
		postAs(t, e, tok, cid, map[string]any{"body": fmt.Sprintf("Filler post number %d", i)})
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := base
		if cursor != "" {
			path += "?cursor=" + cursor
		}
		w, _ := e.do(t, "GET", path, tok, "", nil)
		code, out := e.raw(t, "GET", path, tok, nil)
		if code != 200 {
			t.Fatalf("page: %d", code)
		}
		for _, p := range out.([]any) {
			id := field(p, "id").(string)
			if seen[id] {
				t.Fatalf("duplicate %s", id)
			}
			seen[id] = true
		}
		pages++
		cursor = w.Header().Get("X-Next-Cursor")
		if cursor == "" {
			break
		}
	}
	if len(seen) != 45 || pages != 3 {
		t.Fatalf("paged %d posts in %d pages", len(seen), pages)
	}

	// Pull-to-refresh: newer_than the newest loaded returns only the new post.
	code, out := e.raw(t, "GET", base, tok, nil)
	if code != 200 {
		t.Fatal(code)
	}
	newest := field(out.([]any)[0], "created_at").(string)
	fresh := postAs(t, e, tok, cid, map[string]any{"body": "Brand new after refresh"})
	if r := feedIDs(t, e, tok, base+"?newer_than="+strings.ReplaceAll(newest, "+", "%2B")); len(r) != 1 || r[0] != fresh {
		t.Fatalf("refresh: %v", r)
	}
}

func TestFeedExcludesHiddenMutedBlocked(t *testing.T) {
	e := newSessEnv(t)
	owner, tok := admUser(t, e, "owner@example.com")
	ann, annTok := admUser(t, e, "ann@example.com")
	bob, bobTok := admUser(t, e, "bob@example.com")
	cid, _ := admCommunity(t, e, owner, "Alpha", "public")
	chatJoin(t, e, cid, ann)
	chatJoin(t, e, cid, bob)
	base := "/communities/" + cid + "/posts"
	p1 := postAs(t, e, annTok, cid, map[string]any{"body": "Ann's first post here"})
	p2 := postAs(t, e, bobTok, cid, map[string]any{"body": "Bob's first post here"})
	p3 := postAs(t, e, tok, cid, map[string]any{"body": "Owner post to be hidden"})
	ctx := context.Background()
	if _, err := e.td.Pool.Exec(ctx, `insert into public.content_filters (user_id, kind, post_id) values ($1, 'hide_post', $2)`, owner, p3); err != nil {
		t.Fatal(err)
	}
	if _, err := e.td.Pool.Exec(ctx, `insert into public.content_filters (user_id, kind, muted_user_id) values ($1, 'mute_author', $2)`, owner, ann); err != nil {
		t.Fatal(err)
	}
	if _, err := e.td.Pool.Exec(ctx, `insert into public.blocks (blocker_id, blocked_id) values ($1, $2)`, bob, owner); err != nil {
		t.Fatal(err)
	}
	ids := feedIDs(t, e, tok, base)
	if contains(ids, p1) || contains(ids, p2) || contains(ids, p3) {
		t.Fatalf("filtered posts shown: %v", ids)
	}
	if code, _ := e.raw(t, "GET", "/posts/"+p2, tok, nil); code != 404 {
		t.Fatalf("blocked author's post: %d", code)
	}
}

func TestPostUpdateAndDelete(t *testing.T) {
	e := newSessEnv(t)
	owner, ownerTok := admUser(t, e, "owner@example.com")
	mod, modTok := admUser(t, e, "mod@example.com")
	mem, memTok := admUser(t, e, "member@example.com")
	other, otherTok := admUser(t, e, "other@example.com")
	cid, _ := admCommunity(t, e, owner, "Alpha", "public")
	for _, u := range []string{mod, mem, other} {
		chatJoin(t, e, cid, u)
	}
	setRole(t, e, cid, mod, "moderator")

	pid := postAs(t, e, memTok, cid, map[string]any{"body": "Selling my old bike", "job_tag": "available"})

	// Non-author edit → 403; author edit saves and stamps edited_at.
	if code, _ := e.raw(t, "PATCH", "/posts/"+pid, otherTok, map[string]any{"body": "Hijacked text here"}); code != 403 {
		t.Fatalf("non-author edit: %d", code)
	}
	if code, _ := e.raw(t, "PATCH", "/posts/"+pid, memTok, map[string]any{"body": "short"}); code != 400 {
		t.Fatalf("short edit: %d", code)
	}
	code, out := e.raw(t, "PATCH", "/posts/"+pid, memTok, map[string]any{
		"body": "Selling my old bicycle", "job_tag": "general", "pinned_to_profile": true, "like_count": 99})
	if code != 200 || field(out, "edited_at") == nil || field(out, "job_tag") != "general" ||
		field(out, "pinned_to_profile") != true || field(out, "like_count") != float64(0) {
		t.Fatalf("author edit: %d %v", code, out)
	}
	var inJobs int
	if err := e.td.Pool.QueryRow(context.Background(),
		`select count(*) from public.jobs_feed where post_id = $1`, pid).Scan(&inJobs); err == nil && inJobs != 0 {
		t.Fatalf("general post still in jobs_feed")
	}

	// A member cannot delete another's post; a moderator can.
	if code, _ := e.raw(t, "DELETE", "/posts/"+pid, otherTok, nil); code != 403 {
		t.Fatalf("member delete: %d", code)
	}
	if code, _ := e.raw(t, "DELETE", "/posts/"+pid, modTok, nil); code != 204 {
		t.Fatalf("moderator delete: %d", code)
	}
	if code, out := e.raw(t, "GET", "/posts/"+pid, memTok, nil); code != 404 ||
		!strings.Contains(fmt.Sprint(out), "Post not found") {
		t.Fatalf("deleted get: %d %v", code, out)
	}
	if contains(feedIDs(t, e, memTok, "/communities/"+cid+"/posts"), pid) {
		t.Fatal("deleted post in feed")
	}

	// The author deletes their own post.
	own := postAs(t, e, memTok, cid, map[string]any{"body": "Another one of mine"})
	if code, _ := e.raw(t, "DELETE", "/posts/"+own, memTok, nil); code != 204 {
		t.Fatalf("author delete: %d", code)
	}
	_ = ownerTok
}
