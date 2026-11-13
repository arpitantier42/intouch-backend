package server

import (
	"context"
	"strings"
	"testing"
)

// engEnv: owner and member joined to one public community, plus an outsider.
type engEnv struct {
	*sessEnv
	cid                      string
	owner, mem               string
	ownerTok, memTok, outTok string
}

func newEngEnv(t *testing.T) *engEnv {
	e := newSessEnv(t)
	owner, ownerTok := admUser(t, e, "owner@example.com")
	mem, memTok := admUser(t, e, "member@example.com")
	_, outTok := admUser(t, e, "outsider@example.com")
	cid, _ := admCommunity(t, e, owner, "Alpha", "public")
	chatJoin(t, e, cid, mem)
	return &engEnv{sessEnv: e, cid: cid, owner: owner, mem: mem, ownerTok: ownerTok, memTok: memTok, outTok: outTok}
}

func (e *engEnv) likeCount(t *testing.T, pid string) float64 {
	t.Helper()
	code, out := e.raw(t, "GET", "/posts/"+pid, e.memTok, nil)
	if code != 200 {
		t.Fatalf("get post: %d %v", code, out)
	}
	return field(out, "like_count").(float64)
}

func TestLikesIdempotentAndCounted(t *testing.T) {
	e := newEngEnv(t)
	pid := postAs(t, e.sessEnv, e.ownerTok, e.cid, map[string]any{"body": "Please like this post"})

	// Like twice → count goes up by exactly one; both answers 200.
	var likeID string
	for i := 0; i < 2; i++ {
		code, out := e.raw(t, "PUT", "/likes", e.memTok, map[string]any{"post_id": pid})
		if code != 200 || field(out, "like_count") != float64(1) {
			t.Fatalf("like %d: %d %v", i, code, out)
		}
		likeID = field(out, "id").(string)
	}
	if n := e.likeCount(t, pid); n != 1 {
		t.Fatalf("like_count = %v", n)
	}
	// Another user's like is reflected in the viewer's count on the next read
	// (the trigger writes posts.like_count, which Realtime broadcasts).
	if code, _ := e.raw(t, "PUT", "/likes", e.ownerTok, map[string]any{"post_id": pid}); code != 200 {
		t.Fatal("owner like")
	}
	if n := e.likeCount(t, pid); n != 2 {
		t.Fatalf("like_count after second user = %v", n)
	}
	var pub bool
	if err := e.td.Pool.QueryRow(context.Background(), `select exists (select 1 from pg_publication_tables
		where pubname = 'supabase_realtime' and tablename = 'posts')`).Scan(&pub); err != nil || !pub {
		t.Fatalf("posts not in realtime publication: %v %v", pub, err)
	}

	// Unlike by like id, then again (no-op) → 204, count back to 1.
	for i := 0; i < 2; i++ {
		if code, out := e.raw(t, "DELETE", "/likes/"+likeID, e.memTok, nil); code != 204 {
			t.Fatalf("unlike %d: %d %v", i, code, out)
		}
	}
	if n := e.likeCount(t, pid); n != 1 {
		t.Fatalf("after unlike = %v", n)
	}
	// Unlike by post id works too.
	e.raw(t, "DELETE", "/likes/"+pid, e.ownerTok, nil)
	if n := e.likeCount(t, pid); n != 0 {
		t.Fatalf("after unlike by post id = %v", n)
	}

	// Not visible → 404; bad shape → 400.
	if code, _ := e.raw(t, "PUT", "/likes", e.outTok, map[string]any{"post_id": pid}); code != 404 {
		t.Fatalf("outsider like: %d", code)
	}
	for name, body := range map[string]map[string]any{
		"none": {}, "both": {"post_id": pid, "comment_id": pid}, "bad uuid": {"post_id": "x"},
	} {
		if code, _ := e.raw(t, "PUT", "/likes", e.memTok, body); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}

	// Comment likes count on the comment.
	_, c := e.raw(t, "POST", "/posts/"+pid+"/comments", e.ownerTok, map[string]any{"body": "first"})
	cid := field(c, "id").(string)
	code, out := e.raw(t, "PUT", "/likes", e.memTok, map[string]any{"comment_id": cid})
	if code != 200 || field(out, "like_count") != float64(1) || field(out, "comment_id") != cid {
		t.Fatalf("comment like: %d %v", code, out)
	}
}

func TestCommentsNestedSortedAndValidated(t *testing.T) {
	e := newEngEnv(t)
	pid := postAs(t, e.sessEnv, e.ownerTok, e.cid, map[string]any{"body": "What do you all think"})

	// No comments → empty list ('Be the first to comment').
	code, out := e.raw(t, "GET", "/posts/"+pid+"/comments", e.memTok, nil)
	if code != 200 || len(out.([]any)) != 0 {
		t.Fatalf("empty: %d %v", code, out)
	}

	// Blank / whitespace / too long → 400.
	for name, body := range map[string]map[string]any{
		"missing": {}, "blank": {"body": ""}, "spaces": {"body": "   \n "},
		"long": {"body": strings.Repeat("x", 1001)},
	} {
		if code, _ := e.raw(t, "POST", "/posts/"+pid+"/comments", e.memTok, body); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}

	_, a := e.raw(t, "POST", "/posts/"+pid+"/comments", e.memTok, map[string]any{"body": "Older comment"})
	aID := field(a, "id").(string)
	code, b := e.raw(t, "POST", "/posts/"+pid+"/comments", e.ownerTok, map[string]any{"body": "  Newer comment  "})
	if code != 201 || field(b, "body") != "Newer comment" || field(b, "parent_id") != nil {
		t.Fatalf("create: %d %v", code, b)
	}
	bID := field(b, "id").(string)
	code, r := e.raw(t, "POST", "/posts/"+pid+"/comments", e.ownerTok, map[string]any{"body": "A reply", "parent_id": aID})
	if code != 201 || field(r, "parent_id") != aID {
		t.Fatalf("reply: %d %v", code, r)
	}

	// Recent: newest top-level first; the reply nests under its parent only.
	_, out = e.raw(t, "GET", "/posts/"+pid+"/comments?sort=recent", e.memTok, nil)
	list := out.([]any)
	if len(list) != 2 || field(list[0], "id") != bID || field(list[1], "id") != aID {
		t.Fatalf("recent order: %v", out)
	}
	replies := field(list[1], "replies").([]any)
	if len(replies) != 1 || field(replies[0], "id") != field(r, "id") || len(field(list[0], "replies").([]any)) != 0 {
		t.Fatalf("nesting: %v", out)
	}

	// Relevant: the liked comment first.
	e.raw(t, "PUT", "/likes", e.memTok, map[string]any{"comment_id": bID})
	e.raw(t, "PUT", "/likes", e.ownerTok, map[string]any{"comment_id": bID})
	_, out = e.raw(t, "GET", "/posts/"+pid+"/comments", e.memTok, nil)
	if field(out.([]any)[0], "id") != bID || field(out.([]any)[0], "liked") != true {
		t.Fatalf("relevant: %v", out)
	}

	// Comment count on the post; bad sort 400; outsider 404; parent on another post 400.
	_, p := e.raw(t, "GET", "/posts/"+pid, e.memTok, nil)
	if field(p, "comment_count") != float64(3) {
		t.Fatalf("comment_count: %v", field(p, "comment_count"))
	}
	if code, _ := e.raw(t, "GET", "/posts/"+pid+"/comments?sort=top", e.memTok, nil); code != 400 {
		t.Fatalf("bad sort: %d", code)
	}
	if code, _ := e.raw(t, "GET", "/posts/"+pid+"/comments", e.outTok, nil); code != 404 {
		t.Fatalf("outsider list: %d", code)
	}
	if code, _ := e.raw(t, "POST", "/posts/"+pid+"/comments", e.outTok, map[string]any{"body": "hi"}); code != 404 {
		t.Fatalf("outsider comment: %d", code)
	}
	other := postAs(t, e.sessEnv, e.ownerTok, e.cid, map[string]any{"body": "Another post entirely"})
	if code, _ := e.raw(t, "POST", "/posts/"+other+"/comments", e.memTok, map[string]any{"body": "x", "parent_id": aID}); code != 400 {
		t.Fatalf("cross-post parent: %d", code)
	}
}

func TestPollVoteReplacesAndDocumentPost(t *testing.T) {
	e := newEngEnv(t)
	// A poll with one option is rejected.
	if code, _ := e.raw(t, "POST", "/posts?community_ids="+e.cid, e.ownerTok,
		map[string]any{"body": "Pick one of these", "poll_options": []string{"only"}}); code != 400 {
		t.Fatalf("one-option poll: %d", code)
	}
	pid := postAs(t, e.sessEnv, e.ownerTok, e.cid,
		map[string]any{"body": "Which day suits you", "poll_options": []string{"Sat", "Sun", "Mon"}})

	code, out := e.raw(t, "PUT", "/posts/"+pid+"/vote", e.memTok, map[string]any{"option_index": 0})
	if code != 200 || field(out, "option_index") != float64(0) {
		t.Fatalf("vote: %d %v", code, out)
	}
	first := field(out, "id")
	code, out = e.raw(t, "PUT", "/posts/"+pid+"/vote", e.memTok, map[string]any{"option_index": 2})
	if code != 200 || field(out, "id") != first || field(out, "my_vote") != float64(2) {
		t.Fatalf("revote: %d %v", code, out)
	}
	res := field(out, "poll_results").([]any)
	if res[0] != float64(0) || res[2] != float64(1) {
		t.Fatalf("results: %v", res)
	}
	for name, body := range map[string]map[string]any{
		"out of range": {"option_index": 3}, "negative": {"option_index": -1}, "missing": {},
	} {
		if code, _ := e.raw(t, "PUT", "/posts/"+pid+"/vote", e.memTok, body); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	text := postAs(t, e.sessEnv, e.ownerTok, e.cid, map[string]any{"body": "Just a text post"})
	if code, _ := e.raw(t, "PUT", "/posts/"+text+"/vote", e.memTok, map[string]any{"option_index": 0}); code != 400 {
		t.Fatalf("vote on text: %d", code)
	}
	if code, _ := e.raw(t, "PUT", "/posts/"+pid+"/vote", e.outTok, map[string]any{"option_index": 0}); code != 404 {
		t.Fatalf("outsider vote: %d", code)
	}

	// A document post returns its file URL to members (the client opens it).
	url := "https://files.example.com/agenda.pdf"
	doc := postAs(t, e.sessEnv, e.ownerTok, e.cid,
		map[string]any{"body": "Agenda for the meetup", "media_url": url, "media_kind": "document"})
	_, p := e.raw(t, "GET", "/posts/"+doc, e.memTok, nil)
	if field(p, "media_url") != url || field(p, "kind") != "document" {
		t.Fatalf("document post: %v", p)
	}
}

func savedIDs(t *testing.T, e *engEnv, tok string) map[string]bool {
	t.Helper()
	code, out := e.raw(t, "GET", "/me/bookmarks", tok, nil)
	if code != 200 {
		t.Fatalf("list bookmarks: %d %v", code, out)
	}
	ids := map[string]bool{}
	for _, b := range out.([]any) {
		if p := field(b, "post"); p != nil {
			ids[field(p, "id").(string)] = true
		}
		if j := field(b, "job"); j != nil {
			ids[field(j, "id").(string)] = true
		}
	}
	return ids
}

func TestBookmarksPostsAndJobs(t *testing.T) {
	e := newEngEnv(t)
	pid := postAs(t, e.sessEnv, e.ownerTok, e.cid, map[string]any{"body": "Worth saving for later"})
	keep := postAs(t, e.sessEnv, e.ownerTok, e.cid, map[string]any{"body": "Also worth saving"})
	var jobID string
	if err := e.td.Pool.QueryRow(context.Background(), `insert into public.jobs
		(community_id, poster_id, intent, title, company) values ($1, $2, 'opportunity', 'Backend engineer', 'Acme')
		returning id`, e.cid, e.owner).Scan(&jobID); err != nil {
		t.Fatal(err)
	}

	// Save twice → same row; save a job → the role appears in Saved.
	_, b1 := e.raw(t, "PUT", "/bookmarks", e.memTok, map[string]any{"post_id": pid})
	code, b2 := e.raw(t, "PUT", "/bookmarks", e.memTok, map[string]any{"post_id": pid})
	if code != 200 || field(b1, "id") != field(b2, "id") {
		t.Fatalf("save twice: %d %v %v", code, b1, b2)
	}
	e.raw(t, "PUT", "/bookmarks", e.memTok, map[string]any{"post_id": keep})
	code, jb := e.raw(t, "PUT", "/bookmarks", e.memTok, map[string]any{"job_id": jobID})
	if code != 200 || field(jb, "job_id") != jobID {
		t.Fatalf("save job: %d %v", code, jb)
	}
	saved := savedIDs(t, e, e.memTok)
	if !saved[pid] || !saved[keep] || !saved[jobID] {
		t.Fatalf("saved: %v", saved)
	}
	_, list := e.raw(t, "GET", "/me/bookmarks", e.memTok, nil)
	if j := field(list.([]any)[0], "job"); j == nil || field(j, "title") != "Backend engineer" {
		t.Fatalf("newest first with job title: %v", list)
	}

	// Author deletes the post → it disappears from Saved.
	if code, _ := e.raw(t, "DELETE", "/posts/"+pid, e.ownerTok, nil); code != 204 {
		t.Fatal("delete post")
	}
	if saved := savedIDs(t, e, e.memTok); saved[pid] || !saved[keep] {
		t.Fatalf("deleted post still saved: %v", saved)
	}
	// Un-bookmark (by bookmark id and by job id) → removed from Saved.
	if code, _ := e.raw(t, "DELETE", "/bookmarks/"+field(jb, "id").(string), e.memTok, nil); code != 204 {
		t.Fatal("remove job bookmark")
	}
	if code, _ := e.raw(t, "DELETE", "/bookmarks/"+keep, e.memTok, nil); code != 204 {
		t.Fatal("remove by post id")
	}
	if saved := savedIDs(t, e, e.memTok); len(saved) != 0 {
		t.Fatalf("after removal: %v", saved)
	}

	// Outsider cannot save; bad shape 400; others' Saved stays private.
	if code, _ := e.raw(t, "PUT", "/bookmarks", e.outTok, map[string]any{"post_id": keep}); code != 404 {
		t.Fatalf("outsider post: %d", code)
	}
	if code, _ := e.raw(t, "PUT", "/bookmarks", e.outTok, map[string]any{"job_id": jobID}); code != 404 {
		t.Fatalf("outsider job: %d", code)
	}
	if code, _ := e.raw(t, "PUT", "/bookmarks", e.memTok, map[string]any{"post_id": keep, "job_id": jobID}); code != 400 {
		t.Fatalf("both: %d", code)
	}
	e.raw(t, "PUT", "/bookmarks", e.memTok, map[string]any{"post_id": keep})
	if saved := savedIDs(t, e, e.ownerTok); len(saved) != 0 {
		t.Fatalf("owner sees member's saved: %v", saved)
	}
}

func TestContentFiltersHideMuteUndo(t *testing.T) {
	e := newEngEnv(t)
	feed := "/communities/" + e.cid + "/posts"
	pid := postAs(t, e.sessEnv, e.ownerTok, e.cid, map[string]any{"body": "A post to hide away"})

	// Hide → gone from the feed; Undo → back.
	code, f := e.raw(t, "POST", "/me/content-filters", e.memTok, map[string]any{"kind": "hide_post", "post_id": pid})
	if code != 201 || field(f, "kind") != "hide_post" {
		t.Fatalf("hide: %d %v", code, f)
	}
	if contains(feedIDs(t, e.sessEnv, e.memTok, feed), pid) {
		t.Fatal("hidden post still in feed")
	}
	if code, again := e.raw(t, "POST", "/me/content-filters", e.memTok, map[string]any{"kind": "hide_post", "post_id": pid}); code != 201 || field(again, "id") != field(f, "id") {
		t.Fatalf("hide twice: %d %v", code, again)
	}
	if code, _ := e.raw(t, "DELETE", "/me/content-filters/"+field(f, "id").(string), e.memTok, nil); code != 204 {
		t.Fatal("undo hide")
	}
	if !contains(feedIDs(t, e.sessEnv, e.memTok, feed), pid) {
		t.Fatal("post did not reappear after Undo")
	}

	// Mute author → their new posts stay out of the feed; Undo restores them.
	code, m := e.raw(t, "POST", "/me/content-filters", e.memTok, map[string]any{"kind": "mute_author", "muted_user_id": e.owner})
	if code != 201 || field(m, "muted_user_id") != e.owner {
		t.Fatalf("mute: %d %v", code, m)
	}
	newer := postAs(t, e.sessEnv, e.ownerTok, e.cid, map[string]any{"body": "Posted after the mute"})
	if ids := feedIDs(t, e.sessEnv, e.memTok, feed); contains(ids, newer) || contains(ids, pid) {
		t.Fatalf("muted author's posts in feed: %v", ids)
	}
	if !contains(feedIDs(t, e.sessEnv, e.ownerTok, feed), newer) {
		t.Fatal("mute leaked to the author's own feed")
	}
	if code, _ := e.raw(t, "DELETE", "/me/content-filters/"+e.owner, e.memTok, nil); code != 204 {
		t.Fatal("undo mute by user id")
	}
	if !contains(feedIDs(t, e.sessEnv, e.memTok, feed), newer) {
		t.Fatal("unmuted posts missing")
	}

	// Validation.
	for name, tc := range map[string]struct {
		body map[string]any
		want int
	}{
		"bad kind":     {map[string]any{"kind": "block", "post_id": pid}, 400},
		"self mute":    {map[string]any{"kind": "mute_author", "muted_user_id": e.mem}, 400},
		"wrong field":  {map[string]any{"kind": "hide_post", "muted_user_id": e.owner}, 400},
		"both":         {map[string]any{"kind": "hide_post", "post_id": pid, "muted_user_id": e.owner}, 400},
		"unknown user": {map[string]any{"kind": "mute_author", "muted_user_id": "00000000-0000-0000-0000-000000000001"}, 404},
	} {
		if code, out := e.raw(t, "POST", "/me/content-filters", e.memTok, tc.body); code != tc.want {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, _ := e.raw(t, "POST", "/me/content-filters", e.outTok, map[string]any{"kind": "hide_post", "post_id": pid}); code != 404 {
		t.Fatalf("outsider hide: %d", code)
	}
}

func TestEngagementRequiresBearer(t *testing.T) {
	e := newEngEnv(t)
	for _, r := range [][2]string{{"PUT", "/likes"}, {"GET", "/me/bookmarks"}, {"POST", "/me/content-filters"}} {
		if code, _ := e.raw(t, r[0], r[1], "", map[string]any{}); code != 401 {
			t.Errorf("%s %s: %d", r[0], r[1], code)
		}
	}
}
