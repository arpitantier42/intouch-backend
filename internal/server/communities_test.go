package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func names(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, it := range list {
		out = append(out, field(it, "name").(string))
	}
	return out
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestCreateCommunity(t *testing.T) {
	e := newSessEnv(t)
	owner, tok := admUser(t, e, "owner@example.com")

	// Field validation: nothing is created.
	code, out := e.raw(t, "POST", "/communities", tok, map[string]any{"name": "  ", "location_type": "online"})
	if code != 400 || field(field(out, "error"), "message") != "Enter a community name." {
		t.Fatalf("blank name: %d %v", code, out)
	}
	code, out = e.raw(t, "POST", "/communities", tok, map[string]any{"name": "Pune Club", "location_type": "place"})
	if code != 400 || field(field(out, "error"), "message") != "Enter a city or area." {
		t.Fatalf("no city: %d %v", code, out)
	}
	if code, _ = e.raw(t, "POST", "/communities", tok, map[string]any{"name": "X", "location_type": "place", "city": "Nairobi"}); code != 400 {
		t.Fatalf("city outside list: %d", code)
	}
	var n int
	_ = e.td.Pool.QueryRow(context.Background(), `select count(*) from public.communities`).Scan(&n)
	if n != 0 {
		t.Fatalf("communities after failures: %d", n)
	}
	if code, _ = e.raw(t, "POST", "/communities", "", map[string]any{"name": "X", "location_type": "online"}); code != 401 {
		t.Fatalf("visitor: %d", code)
	}

	// Defaults, owner membership via direct, online without city.
	key := "6f1c1d2e-8d6a-4a57-9a50-3b1f0e3c9a11"
	code, out = e.raw(t, "POST", "/communities", tok, map[string]any{"name": "Agarwal Samaj Jaipur", "location_type": "place", "city": "jaipur", "creation_key": key})
	if code != 201 || field(out, "visibility") != "private" || field(out, "handle") != "agarwal-samaj-jaipur" ||
		field(out, "emoji") == "" || field(out, "cover_gradient") == "" || field(out, "city") != "Jaipur" {
		t.Fatalf("create: %d %v", code, out)
	}
	cid := field(out, "id").(string)
	var role, via, status string
	_ = e.td.Pool.QueryRow(context.Background(), `select role, via, status from public.memberships where community_id=$1 and user_id=$2`, cid, owner).Scan(&role, &via, &status)
	if role != "owner" || via != "direct" || status != "joined" {
		t.Fatalf("membership %s %s %s", role, via, status)
	}
	// Same key again → 200 with the same community.
	code, out = e.raw(t, "POST", "/communities", tok, map[string]any{"name": "Agarwal Samaj Jaipur", "location_type": "place", "city": "Jaipur", "creation_key": key})
	if code != 200 || field(out, "id") != cid {
		t.Fatalf("idempotent: %d %v", code, out)
	}
	// Handle collision, long names and symbol-only names.
	_, out = e.raw(t, "POST", "/communities", tok, map[string]any{"name": "Agarwal Samaj Jaipur", "location_type": "online"})
	if field(out, "handle") != "agarwal-samaj-jaipur-2" || field(out, "city") != nil {
		t.Fatalf("collision: %v", out)
	}
	_, out = e.raw(t, "POST", "/communities", tok, map[string]any{"name": strings.Repeat("Bangalore Runners ", 4)[:60], "location_type": "online"})
	if h := field(out, "handle").(string); len(h) > 40 || h == "" {
		t.Fatalf("long handle %q", h)
	}
	_, out = e.raw(t, "POST", "/communities", tok, map[string]any{"name": "!!! ***", "location_type": "online"})
	if field(out, "handle") != "community" {
		t.Fatalf("symbol handle: %v", out)
	}

	// Double-tap: concurrent requests with one key create one community.
	key2 := "0c7a9b55-3a3f-4d3e-8b8b-9a8f2c1d0e22"
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = e.raw(t, "POST", "/communities", tok, map[string]any{"name": "Tap Tap", "location_type": "online", "creation_key": key2})
		}(i)
	}
	wg.Wait()
	_ = e.td.Pool.QueryRow(context.Background(), `select count(*) from public.communities where creation_key=$1`, key2).Scan(&n)
	if n != 1 || codes[0]+codes[1] != 401 {
		t.Fatalf("double tap: n=%d codes=%v", n, codes)
	}
}

func TestUpdateCommunity(t *testing.T) {
	e := newSessEnv(t)
	owner, ownerTok := admUser(t, e, "owner@example.com")
	admin, adminTok := admUser(t, e, "admin@example.com")
	_, memberTok := admUser(t, e, "member@example.com")
	member, _ := admUser(t, e, "m2@example.com")
	_ = member
	cid, _ := admCommunity(t, e, owner, "Runners", "public")
	_, err := e.td.Pool.Exec(context.Background(), `insert into public.memberships (community_id, user_id, role, status, via) values ($1, $2, 'admin', 'joined', 'direct')`, cid, admin)
	if err != nil {
		t.Fatal(err)
	}
	base := "/communities/" + cid

	code, out := e.raw(t, "PATCH", base, ownerTok, map[string]any{"category": "Sports & Play", "interests": []string{"cricket", " Cricket ", "", "Running"}})
	if code != 200 || strings.Join(toStrings(field(out, "interests")), ",") != "Sports,Cricket,Running" {
		t.Fatalf("interests: %d %v", code, out)
	}
	if code, _ = e.raw(t, "PATCH", base, ownerTok, map[string]any{"interests": []string{"Knitting"}}); code != 400 {
		t.Fatalf("limited unlisted: %d", code)
	}
	code, out = e.raw(t, "PATCH", base, ownerTok, map[string]any{"category": "Club", "interests": []string{"Knitting"}})
	if code != 200 || field(out, "category") != "Club" || !has(toStrings(field(out, "interests")), "Knitting") {
		t.Fatalf("open custom: %d %v", code, out)
	}
	if code, _ = e.raw(t, "PATCH", base, ownerTok, map[string]any{"rules": []string{" "}}); code != 400 {
		t.Fatalf("zero rules: %d", code)
	}
	_, out = e.raw(t, "PATCH", base, ownerTok, map[string]any{"rules": []string{"Be kind", "No spam"}})
	_, out = e.raw(t, "PATCH", base, ownerTok, map[string]any{"rules": []string{"No spam", "Be kind"}})
	if strings.Join(toStrings(field(out, "rules")), "|") != "No spam|Be kind" {
		t.Fatalf("rules order: %v", out)
	}
	if code, _ = e.raw(t, "PATCH", base, ownerTok, map[string]any{"logo_url": "https://x.example/a/logo.pdf"}); code != 415 {
		t.Fatalf("pdf logo: %d", code)
	}
	if code, _ = e.raw(t, "PATCH", base, ownerTok, map[string]any{"logo_url": "https://x.example/a/logo.png"}); code != 200 {
		t.Fatalf("png logo: %d", code)
	}
	if code, _ = e.raw(t, "PATCH", base, ownerTok, map[string]any{"questions": []map[string]any{{"label": "CV", "kind": "file"}}}); code != 400 {
		t.Fatalf("file question: %d", code)
	}
	if code, _ = e.raw(t, "PATCH", base, ownerTok, map[string]any{"questions": []map[string]any{{"label": "Pick", "kind": "single_choice", "options": []string{}}}}); code != 400 {
		t.Fatalf("choice w/o options: %d", code)
	}
	code, out = e.raw(t, "PATCH", base, ownerTok, map[string]any{"questions": []map[string]any{{"label": "Level", "kind": "single_choice", "options": []string{"Beginner", "Pro"}, "required": true, "why_we_ask": "Pairing"}}, "open_to_collaborate": true})
	if code != 200 || len(field(out, "questions").([]any)) != 1 || field(out, "open_to_collaborate") != true {
		t.Fatalf("question save: %d %v", code, out)
	}
	// Admins edit profile and rules, not settings; members edit nothing.
	if code, _ = e.raw(t, "PATCH", base, adminTok, map[string]any{"rules": []string{"Admin rule"}}); code != 200 {
		t.Fatalf("admin rules: %d", code)
	}
	for _, body := range []map[string]any{{"open_to_collaborate": false}, {"visibility": "private"}, {"invite_permission": "all_members"}} {
		if code, _ = e.raw(t, "PATCH", base, adminTok, body); code != 403 {
			t.Fatalf("admin settings %v: %d", body, code)
		}
	}
	if code, _ = e.raw(t, "PATCH", base, memberTok, map[string]any{"rules": []string{"x"}}); code != 403 {
		t.Fatalf("non-member: %d", code)
	}
	// Reload keeps open_to_collaborate.
	_, out = e.raw(t, "GET", base, ownerTok, nil)
	if field(out, "open_to_collaborate") != true {
		t.Fatalf("reload: %v", out)
	}
}

func toStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		out = append(out, x.(string))
	}
	return out
}

func TestCommunityOverviewAndListing(t *testing.T) {
	e := newSessEnv(t)
	owner, ownerTok := admUser(t, e, "owner@example.com")
	other, otherTok := admUser(t, e, "other@example.com")
	pub, _ := admCommunity(t, e, owner, "Sports Hub", "public")
	priv, _ := admCommunity(t, e, owner, "Hidden Circle", "private")
	ctx := context.Background()
	if _, err := e.td.Pool.Exec(ctx, `update public.communities set category='Sports & Play' where id=$1`, pub); err != nil {
		t.Fatal(err)
	}

	// Owner checklist starts at 0 and the first post makes it 20%.
	_, out := e.raw(t, "GET", "/communities/"+pub, ownerTok, nil)
	if field(field(out, "checklist"), "percent_complete") != float64(0) || field(out, "role_badge") != "You manage" {
		t.Fatalf("checklist: %v", out)
	}
	var pid string
	if err := e.td.Pool.QueryRow(ctx, `insert into public.posts (author_id, kind, body) values ($1, 'text', 'Hello runners of the city') returning id`, owner).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.td.Pool.Exec(ctx, `insert into public.post_targets (post_id, community_id) values ($1, $2)`, pid, pub); err != nil {
		t.Fatal(err)
	}
	_, out = e.raw(t, "GET", "/communities/"+pub, ownerTok, nil)
	if field(field(out, "checklist"), "percent_complete") != float64(20) {
		t.Fatalf("checklist after post: %v", out)
	}

	// Non-member: counts only, no checklist, Join shown, Members only.
	code, out := e.raw(t, "GET", "/communities/"+pub, otherTok, nil)
	if code != 200 || field(out, "checklist") != nil || field(out, "join_action") != "join" ||
		field(out, "content_message") != "Members only" || field(field(out, "whats_inside"), "posts_per_week") != float64(1) {
		t.Fatalf("non-member overview: %d %v", code, out)
	}
	if strings.Contains(string(mustJSON(out)), "Hello runners") {
		t.Fatal("post content leaked")
	}
	// Private: 403 without a code or with a tampered one, 200 with a valid one.
	for _, q := range []string{"", "?invite_code=deadbeef"} {
		code, out = e.raw(t, "GET", "/communities/"+priv+q, otherTok, nil)
		if code != 403 || field(field(out, "error"), "message") != "Invitation required" {
			t.Fatalf("private %q: %d %v", q, code, out)
		}
	}
	var invCode string
	if err := e.td.Pool.QueryRow(ctx, `insert into public.invitations (community_id, code, channel, created_by, expires_at, max_uses)
		values ($1, 'abcdefabcdefabcdefabcdefabcdef12', 'link', $2, now() + interval '1 day', 5) returning code`, priv, owner).Scan(&invCode); err != nil {
		t.Fatal(err)
	}
	code, out = e.raw(t, "GET", "/communities/"+priv+"?invite_code="+invCode, otherTok, nil)
	if code != 200 || field(out, "join_action") != "accept_invite" {
		t.Fatalf("private with code: %d %v", code, out)
	}
	if code, _ = e.raw(t, "GET", "/communities/00000000-0000-0000-0000-000000000000", otherTok, nil); code != 404 {
		t.Fatalf("unknown: %d", code)
	}

	// Explore and search: public only.
	for _, view := range []string{"trending", "popular", "recommended"} {
		_, out = e.raw(t, "GET", "/communities?view="+view, otherTok, nil)
		if l := names(out); !has(l, "Sports Hub") || has(l, "Hidden Circle") {
			t.Fatalf("%s: %v", view, l)
		}
	}
	_, out = e.raw(t, "GET", "/communities?view=search&q=Hidden%20Circle", otherTok, nil)
	if len(names(out)) != 0 {
		t.Fatalf("private searchable: %v", out)
	}
	_, out = e.raw(t, "GET", "/communities?view=search&q=sports", otherTok, nil)
	if !has(names(out), "Sports Hub") {
		t.Fatalf("category search: %v", out)
	}
	if code, _ = e.raw(t, "GET", "/communities", otherTok, nil); code != 400 {
		t.Fatalf("no view: %d", code)
	}

	// Pending is not in mine; once accepted it is, and it leaves recommended.
	if code, _ := e.raw(t, "POST", "/communities/"+pub+"/join-requests", otherTok, nil); code != 400 && code != 201 {
		t.Fatalf("join: %d", code)
	}
	if _, err := e.td.Pool.Exec(ctx, `insert into public.memberships (community_id, user_id, status, via) values ($1, $2, 'pending', 'request') on conflict do nothing`, pub, other); err != nil {
		t.Fatal(err)
	}
	_, out = e.raw(t, "GET", "/communities?view=mine", otherTok, nil)
	if has(names(out), "Sports Hub") {
		t.Fatal("pending listed in mine")
	}
	if _, err := e.td.Pool.Exec(ctx, `update public.memberships set status='joined', reviewed_by=$3 where community_id=$1 and user_id=$2`, pub, other, owner); err != nil {
		t.Fatal(err)
	}
	_, out = e.raw(t, "GET", "/communities?view=mine", otherTok, nil)
	if !has(names(out), "Sports Hub") || field(out.([]any)[0], "role_badge") != "Member of" {
		t.Fatalf("mine after accept: %v", out)
	}
	_, out = e.raw(t, "GET", "/communities?view=recommended", otherTok, nil)
	if has(names(out), "Sports Hub") {
		t.Fatal("joined community recommended")
	}
	// Welcome shows once.
	_, out = e.raw(t, "GET", "/communities/"+pub, otherTok, nil)
	if field(out, "show_welcome") != true {
		t.Fatalf("welcome: %v", out)
	}
	_, out = e.raw(t, "GET", "/communities/"+pub, otherTok, nil)
	if field(out, "show_welcome") != false {
		t.Fatalf("welcome twice: %v", out)
	}

	// Switching to private removes it from every Explore section.
	e.raw(t, "PATCH", "/communities/"+pub, ownerTok, map[string]any{"visibility": "private"})
	_, third := admUser(t, e, "third@example.com")
	for _, view := range []string{"trending", "popular", "recommended"} {
		_, out = e.raw(t, "GET", "/communities?view="+view, third, nil)
		if has(names(out), "Sports Hub") {
			t.Fatalf("%s lists private", view)
		}
	}
}

func TestDeleteAndRestoreCommunity(t *testing.T) {
	e := newSessEnv(t)
	owner, ownerTok := admUser(t, e, "owner@example.com")
	_, otherTok := admUser(t, e, "other@example.com")
	cid, _ := admCommunity(t, e, owner, "Book Club", "public")
	base := "/communities/" + cid

	if code, _ := e.raw(t, "DELETE", base+"?confirm_name=Book", ownerTok, nil); code != 400 {
		t.Fatalf("unconfirmed: %d", code)
	}
	if code, _ := e.raw(t, "GET", base, ownerTok, nil); code != 200 {
		t.Fatalf("still there: %d", code)
	}
	if code, _ := e.raw(t, "DELETE", base+"?confirm_name=Book%20Club", otherTok, nil); code != 403 {
		t.Fatalf("non-owner delete: %d", code)
	}
	code, out := e.raw(t, "DELETE", base+"?confirm_name=Book%20Club", ownerTok, nil)
	if code != 202 || field(out, "purge_at") == nil {
		t.Fatalf("delete: %d %v", code, out)
	}
	if code, _ := e.raw(t, "GET", base, ownerTok, nil); code != 404 {
		t.Fatalf("deleted overview: %d", code)
	}
	if code, _ := e.raw(t, "POST", base+"/restore", otherTok, nil); code != 403 {
		t.Fatalf("non-owner restore: %d", code)
	}
	code, out = e.raw(t, "POST", base+"/restore", ownerTok, nil)
	if code != 200 || field(out, "deleted_at") != nil || field(out, "member_count") != float64(1) {
		t.Fatalf("restore: %d %v", code, out)
	}
	e.raw(t, "DELETE", base+"?confirm_name=Book%20Club", ownerTok, nil)
	if _, err := e.td.Pool.Exec(context.Background(), `update public.communities set deleted_at = now() - interval '15 days' where id=$1`, cid); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.raw(t, "POST", base+"/restore", ownerTok, nil); code != 410 {
		t.Fatalf("late restore: %d", code)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
