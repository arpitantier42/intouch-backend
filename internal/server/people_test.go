package server

import (
	"context"
	"testing"
)

// setProfile updates profile columns directly (test setup).
func setProfile(t *testing.T, e *sessEnv, uid, set string, args ...any) {
	t.Helper()
	if _, err := e.td.Pool.Exec(context.Background(),
		`update public.profiles set `+set+` where id = $1`, append([]any{uid}, args...)...); err != nil {
		t.Fatal(err)
	}
}

func list(v any) []any { l, _ := v.([]any); return l }

func hasID(v any, id string) bool {
	for _, x := range list(v) {
		if field(x, "id") == id {
			return true
		}
	}
	return false
}

func connect(t *testing.T, e *sessEnv, fromTok, toID, toTok string) {
	t.Helper()
	code, req := e.raw(t, "POST", "/connections", fromTok, map[string]any{"addressee_id": toID})
	if code != 201 {
		t.Fatalf("connect: %d %v", code, req)
	}
	if code, out := e.raw(t, "PATCH", "/connections/"+field(req, "id").(string)+"?action=accept", toTok, nil); code != 200 {
		t.Fatalf("accept: %d %v", code, out)
	}
}

func TestPeopleSearch(t *testing.T) {
	e, _ := newMsgEnv(t)
	me, tok := admUser(t, e, "me@example.com")
	ann, aTok := admUser(t, e, "ann@example.com")
	ben, _ := admUser(t, e, "ben@example.com")
	cat, _ := admUser(t, e, "cat@example.com")
	dan, dTok := admUser(t, e, "dan@example.com")
	out, oTok := admUser(t, e, "outsider@example.com")

	cid, _ := admCommunity(t, e, me, "Alumni", "private")
	other, _ := admCommunity(t, e, out, "Elsewhere", "private")
	for _, u := range []string{ann, ben, cat, dan} {
		chatJoin(t, e, cid, u)
	}
	setProfile(t, e, ann, `skills = '["Golang","SQL"]', city = 'Pune', company = 'Acme'`)
	setProfile(t, e, ben, `appear_in_search = false`)
	setProfile(t, e, cat, `city = 'Pune', hide_city = true`)
	setProfile(t, e, dan, `city = 'pune '`)

	// Validation.
	if code, _ := e.raw(t, "GET", "/people", tok, nil); code != 400 {
		t.Fatalf("no mode: %d", code)
	}
	if code, _ := e.raw(t, "GET", "/people?mode=members", tok, nil); code != 400 {
		t.Fatalf("members without community: %d", code)
	}
	if code, _ := e.raw(t, "GET", "/people?mode=members&community_id="+cid+"&filter=gps", tok, nil); code != 400 {
		t.Fatalf("bad filter: %d", code)
	}
	if code, _ := e.raw(t, "GET", "/people?mode=members&community_id="+other, tok, nil); code != 403 {
		t.Fatalf("not joined: %d", code)
	}

	// All Members: self and appear_in_search=false excluded.
	_, mem := e.raw(t, "GET", "/people?mode=members&community_id="+cid, tok, nil)
	if !hasID(mem, ann) || !hasID(mem, cat) || !hasID(mem, dan) || hasID(mem, ben) || hasID(mem, me) || hasID(mem, out) {
		t.Fatalf("members: %v", mem)
	}
	// Hidden city is not exposed in cards.
	for _, x := range list(mem) {
		if field(x, "id") == cat && field(x, "city") != nil {
			t.Fatalf("hidden city leaked: %v", x)
		}
	}
	// Skill search.
	_, res := e.raw(t, "GET", "/people?mode=members&community_id="+cid+"&q=golang", tok, nil)
	if len(list(res)) != 1 || !hasID(res, ann) {
		t.Fatalf("skill search: %v", res)
	}
	// Same company with no company on my profile → nothing.
	_, res = e.raw(t, "GET", "/people?mode=members&community_id="+cid+"&filter=same_company", tok, nil)
	if len(list(res)) != 0 {
		t.Fatalf("same_company without company: %v", res)
	}
	// Near me uses the profile city (no GPS); hidden cities are skipped.
	setProfile(t, e, me, `city = 'PUNE', company = 'acme'`)
	_, res = e.raw(t, "GET", "/people?mode=members&community_id="+cid+"&filter=near_me", tok, nil)
	if !hasID(res, ann) || !hasID(res, dan) || hasID(res, cat) || len(list(res)) != 2 {
		t.Fatalf("near_me: %v", res)
	}
	_, res = e.raw(t, "GET", "/people?mode=members&community_id="+cid+"&filter=same_company", tok, nil)
	if len(list(res)) != 1 || !hasID(res, ann) {
		t.Fatalf("same_company: %v", res)
	}

	// Suggested: shares a community, not connected; outsider never.
	_, sug := e.raw(t, "GET", "/people?mode=suggested", tok, nil)
	if !hasID(sug, ann) || hasID(sug, out) || hasID(sug, ben) {
		t.Fatalf("suggested: %v", sug)
	}
	if field(list(sug)[0], "id") != ann {
		t.Fatalf("ann (city+company+community) should rank first: %v", sug)
	}
	connect(t, e, tok, ann, aTok)
	_, sug = e.raw(t, "GET", "/people?mode=suggested", tok, nil)
	if hasID(sug, ann) {
		t.Fatalf("connected user suggested: %v", sug)
	}

	// Connections, scoped to the community for the new-chat / share picker.
	_, con := e.raw(t, "GET", "/people?mode=connections", tok, nil)
	if len(list(con)) != 1 || !hasID(con, ann) || field(list(con)[0], "connection_state") != "connected" {
		t.Fatalf("connections: %v", con)
	}
	_, con = e.raw(t, "GET", "/people?mode=connections&community_id="+cid, tok, nil)
	if !hasID(con, ann) {
		t.Fatalf("scoped connections: %v", con)
	}
	// A connection who is not in the community is not listed for it.
	chatJoin(t, e, other, me)
	connect(t, e, tok, out, oTok)
	_, con = e.raw(t, "GET", "/people?mode=connections&community_id="+cid, tok, nil)
	if hasID(con, out) || !hasID(con, ann) {
		t.Fatalf("non-member listed for community share: %v", con)
	}
	_, con = e.raw(t, "GET", "/people?mode=connections", tok, nil)
	if !hasID(con, out) {
		t.Fatalf("unscoped connections: %v", con)
	}

	// Blocked users are not listed anywhere.
	if code, _ := e.raw(t, "POST", "/me/blocks", dTok, map[string]any{"blocked_id": me}); code != 201 {
		t.Fatalf("block: %d", code)
	}
	_, mem = e.raw(t, "GET", "/people?mode=members&community_id="+cid, tok, nil)
	if hasID(mem, dan) {
		t.Fatalf("blocked member listed: %v", mem)
	}
	_, sug = e.raw(t, "GET", "/people?mode=suggested", tok, nil)
	if hasID(sug, dan) {
		t.Fatalf("blocked suggested: %v", sug)
	}

	// Invitable: excludes joined members and self.
	_, inv := e.raw(t, "GET", "/people?mode=invitable&community_id="+cid, tok, nil)
	if hasID(inv, ann) || hasID(inv, me) || hasID(inv, cat) || !hasID(inv, out) {
		t.Fatalf("invitable: %v", inv)
	}
	_, inv = e.raw(t, "GET", "/people?mode=invitable&community_id="+cid+"&q=ann", tok, nil)
	if len(list(inv)) != 0 {
		t.Fatalf("member searchable in invitable: %v", inv)
	}
	// A plain member cannot invite (invite_permission = admins).
	if code, _ := e.raw(t, "GET", "/people?mode=invitable&community_id="+cid, aTok, nil); code != 403 {
		t.Fatalf("member invitable: %d", code)
	}
}

func TestMemberProfileMentorshipFollow(t *testing.T) {
	e, fp := newMsgEnv(t)
	me, tok := admUser(t, e, "me@example.com")
	ann, aTok := admUser(t, e, "ann@example.com")
	bob, bTok := admUser(t, e, "bob@example.com")
	out, _ := admUser(t, e, "out@example.com")

	c1, _ := admCommunity(t, e, me, "Alpha", "private")
	c2, _ := admCommunity(t, e, ann, "Beta", "private")
	c3, _ := admCommunity(t, e, out, "Gamma", "private")
	chatJoin(t, e, c1, ann)
	chatJoin(t, e, c1, bob)
	chatJoin(t, e, c3, ann) // ann is in Gamma; I am not
	_ = c2
	setProfile(t, e, ann, `city = 'Pune', hide_city = true, bio = 'Hi'`)

	// No shared community → 404; unknown / bad id → 404.
	if code, _ := e.raw(t, "GET", "/users/"+out, tok, nil); code != 404 {
		t.Fatalf("no shared community: %d", code)
	}
	if code, _ := e.raw(t, "GET", "/users/not-a-uuid", tok, nil); code != 404 {
		t.Fatalf("bad id: %d", code)
	}

	code, p := e.raw(t, "GET", "/users/"+ann, tok, nil)
	if code != 200 || field(p, "full_name") == nil || field(p, "bio") != "Hi" {
		t.Fatalf("profile: %d %v", code, p)
	}
	if _, ok := p.(map[string]any)["city"]; ok {
		t.Fatalf("hidden city present: %v", p)
	}
	for _, k := range []string{"phone", "two_factor_enabled", "login_alerts", "deletion_reason"} {
		if _, ok := p.(map[string]any)[k]; ok {
			t.Fatalf("private field %s exposed", k)
		}
	}
	shared := list(field(p, "shared_communities"))
	if len(shared) != 1 || field(shared[0], "id") != c1 {
		t.Fatalf("shared communities: %v", shared)
	}
	if field(p, "connection_state") != "idle" || field(p, "primary_action") != "connect" || field(p, "can_message") != false {
		t.Fatalf("idle actions: %v", p)
	}

	// Connected → Message replaces Connect; mutuals counted.
	connect(t, e, tok, ann, aTok)
	connect(t, e, tok, bob, bTok)
	connect(t, e, aTok, bob, bTok)
	_, p = e.raw(t, "GET", "/users/"+ann, tok, nil)
	if field(p, "primary_action") != "message" || field(p, "can_message") != true || field(p, "connection_id") == nil {
		t.Fatalf("connected actions: %v", p)
	}
	mut := field(p, "mutual_connections")
	if field(mut, "count") != float64(1) || field(list(field(mut, "items"))[0], "id") != bob {
		t.Fatalf("mutuals: %v", mut)
	}

	// Mentorship: topic required; recipient notified.
	if code, out := e.raw(t, "POST", "/users/"+ann+"/mentorship-requests", tok, map[string]any{"message": "hi"}); code != 400 ||
		field(field(field(out, "error"), "fields"), "topic") == nil {
		t.Fatalf("no topic: %d %v", code, out)
	}
	code, mr := e.raw(t, "POST", "/users/"+ann+"/mentorship-requests", tok, map[string]any{"topic": "career", "message": "Advice?"})
	if code != 201 || field(mr, "status") != "open" || field(mr, "recipient_id") != ann || field(mr, "requester_id") != me {
		t.Fatalf("mentorship: %d %v", code, mr)
	}
	var kind, rid string
	if err := e.td.Pool.QueryRow(context.Background(),
		`select kind, payload->>'mentorship_request_id' from public.notifications where user_id = $1 and kind = 'mentorship'`, ann).
		Scan(&kind, &rid); err != nil || rid != field(mr, "id") {
		t.Fatalf("notification: %v %s", err, rid)
	}
	if n := fp.last(t, 1); len(n.UserIDs) != 1 || n.UserIDs[0] != ann || n.Data["mentorship_request_id"] != rid {
		t.Fatalf("push: %+v", n)
	}

	// Follow: idempotent, follower count +1; unfollow 204.
	code, f := e.raw(t, "PUT", "/users/"+ann+"/follow", tok, nil)
	if code != 200 || field(f, "followee_id") != ann {
		t.Fatalf("follow: %d %v", code, f)
	}
	if code, f2 := e.raw(t, "PUT", "/users/"+ann+"/follow", tok, nil); code != 200 || field(f2, "id") != field(f, "id") {
		t.Fatalf("follow again: %d %v", code, f2)
	}
	_, p = e.raw(t, "GET", "/users/"+ann, tok, nil)
	if fo := field(p, "follow"); field(fo, "followers") != float64(1) || field(fo, "is_following") != true {
		t.Fatalf("follow counts: %v", fo)
	}
	if code, _ := e.raw(t, "DELETE", "/users/"+ann+"/follow", tok, nil); code != 204 {
		t.Fatalf("unfollow: %d", code)
	}
	_, p = e.raw(t, "GET", "/users/"+ann, tok, nil)
	if field(field(p, "follow"), "followers") != float64(0) {
		t.Fatalf("after unfollow: %v", p)
	}

	// Connections-only: Follow unavailable (403, can_follow false), and a
	// non-connection sees the limited card.
	setProfile(t, e, bob, `profile_visibility = 'connections', bio = 'secret'`)
	if code, _ := e.raw(t, "PUT", "/users/"+bob+"/follow", tok, nil); code != 403 {
		t.Fatalf("follow connections-only: %d", code)
	}
	_, p = e.raw(t, "GET", "/users/"+bob, tok, nil)
	if field(field(p, "follow"), "can_follow") != false || field(p, "bio") != "secret" {
		t.Fatalf("connected view of connections-only: %v", p)
	}
	if code, _ := e.raw(t, "DELETE", "/connections/"+field(p, "connection_id").(string), tok, nil); code != 204 {
		t.Fatalf("remove connection: %d", code)
	}
	_, p = e.raw(t, "GET", "/users/"+bob, tok, nil)
	if field(p, "limited") != true || field(p, "bio") != nil {
		t.Fatalf("limited view: %v", p)
	}

	// Blocked either way → 404, and follow forbidden.
	if code, _ := e.raw(t, "POST", "/me/blocks", aTok, map[string]any{"blocked_id": me}); code != 201 {
		t.Fatal("block")
	}
	if code, _ := e.raw(t, "GET", "/users/"+ann, tok, nil); code != 404 {
		t.Fatalf("blocked profile: %d", code)
	}
	if code, _ := e.raw(t, "GET", "/users/"+me, aTok, nil); code != 404 {
		t.Fatalf("blocker view: %d", code)
	}
	if code, _ := e.raw(t, "PUT", "/users/"+ann+"/follow", tok, nil); code != 403 {
		t.Fatalf("follow blocked: %d", code)
	}
}

// Picking a person in New Chat reuses the existing DM.
func TestNewChatReusesDirectThread(t *testing.T) {
	e, _ := newMsgEnv(t)
	me, tok := admUser(t, e, "me@example.com")
	ann, _ := admUser(t, e, "ann@example.com")
	cid, _ := admCommunity(t, e, me, "Alpha", "private")
	chatJoin(t, e, cid, ann)
	_, a := e.raw(t, "POST", "/communities/"+cid+"/threads/direct?user_id="+ann, tok, nil)
	_, b := e.raw(t, "POST", "/communities/"+cid+"/threads/direct?user_id="+ann, tok, nil)
	if field(a, "id") == nil || field(a, "id") != field(b, "id") {
		t.Fatalf("duplicate thread: %v %v", a, b)
	}
}
