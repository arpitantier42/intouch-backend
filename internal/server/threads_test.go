package server

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// chatJoin makes uid a joined member of cid directly (test setup).
func chatJoin(t *testing.T, e *sessEnv, cid, uid string) {
	t.Helper()
	if _, err := e.td.Pool.Exec(context.Background(),
		`insert into public.memberships (community_id, user_id, role, status, via) values ($1, $2, 'member', 'joined', 'direct')`,
		cid, uid); err != nil {
		t.Fatal(err)
	}
}

// chatSend inserts a message as the service would (send RPC is a later ticket).
func chatSend(t *testing.T, e *sessEnv, tid, uid, kind, body, url string) {
	t.Helper()
	var u any
	if url != "" {
		u = url
	}
	if _, err := e.td.Pool.Exec(context.Background(),
		`insert into public.messages (thread_id, sender_id, kind, body, attachment_url) values ($1, $2, $3, nullif($4, ''), $5)`,
		tid, uid, kind, body, u); err != nil {
		t.Fatal(err)
	}
}

func ids(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		out = append(out, field(x, "id").(string))
	}
	return out
}

func TestChatThreads(t *testing.T) {
	e := newSessEnv(t)
	alice, aTok := admUser(t, e, "alice@example.com")
	bob, bTok := admUser(t, e, "bob@example.com")
	carol, cTok := admUser(t, e, "carol@example.com")
	dave, _ := admUser(t, e, "dave@example.com")

	a, _ := admCommunity(t, e, alice, "Alpha", "private")
	b, _ := admCommunity(t, e, alice, "Beta", "private")
	chatJoin(t, e, a, bob)
	chatJoin(t, e, b, bob)
	chatJoin(t, e, a, carol)

	// DM with bob in A and in B: two distinct threads; reopening reuses.
	code, dmA := e.raw(t, "POST", "/communities/"+a+"/threads/direct?user_id="+bob, aTok, nil)
	if code != 200 || field(dmA, "kind") != "dm" {
		t.Fatalf("open dm A: %d %v", code, dmA)
	}
	_, dmB := e.raw(t, "POST", "/communities/"+b+"/threads/direct?user_id="+bob, aTok, nil)
	if field(dmA, "id") == field(dmB, "id") {
		t.Fatal("same thread across communities")
	}
	code, again := e.raw(t, "POST", "/communities/"+a+"/threads/direct?user_id="+alice, bTok, nil)
	if code != 200 || field(again, "id") != field(dmA, "id") {
		t.Fatalf("reopen from other side: %d %v", code, again)
	}
	dmAID := field(dmA, "id").(string)

	// Non-member of the community: rejected (403); self: 400.
	if code, out := e.raw(t, "POST", "/communities/"+a+"/threads/direct?user_id="+dave, aTok, nil); code != 403 {
		t.Fatalf("dm non-member: %d %v", code, out)
	}
	if code, _ := e.raw(t, "POST", "/communities/"+b+"/threads/direct?user_id="+alice, cTok, nil); code != 403 {
		t.Fatalf("caller not joined: %d", code)
	}

	// Groups: name and members required; non-member rejected.
	g := "/communities/" + a + "/threads/groups"
	if code, out := e.raw(t, "POST", g+"?member_ids="+bob, aTok, map[string]string{"name": "  "}); code != 400 || field(field(out, "error"), "fields").(map[string]any)["name"] == nil {
		t.Fatalf("blank name: %d %v", code, out)
	}
	if code, _ := e.raw(t, "POST", g, aTok, map[string]string{"name": "Crew"}); code != 400 {
		t.Fatalf("zero members: %d", code)
	}
	if code, _ := e.raw(t, "POST", g+"?member_ids="+dave, aTok, map[string]string{"name": "Crew"}); code != 403 {
		t.Fatalf("non-member in group: %d", code)
	}
	code, grp := e.raw(t, "POST", g+"?member_ids="+bob, aTok, map[string]string{"name": "Crew", "description": "Core team"})
	if code != 201 || field(grp, "kind") != "group" {
		t.Fatalf("create group: %d %v", code, grp)
	}
	gid := field(grp, "id").(string)
	_, info := e.raw(t, "GET", "/threads/"+gid, aTok, nil)
	if field(info, "member_count") != float64(2) {
		t.Fatalf("count before add: %v", info)
	}
	if code, _ := e.raw(t, "POST", "/threads/"+gid+"/participants", aTok, map[string]string{"user_id": dave}); code != 403 {
		t.Fatalf("add non-member: %d", code)
	}
	if code, out := e.raw(t, "POST", "/threads/"+gid+"/participants", aTok, map[string]string{"user_id": carol}); code != 201 || field(out, "user_id") != carol {
		t.Fatalf("add carol: %d %v", code, out)
	}
	_, info = e.raw(t, "GET", "/threads/"+gid, aTok, nil)
	if field(info, "member_count") != float64(3) {
		t.Fatalf("count after add: %v", info)
	}
	if _, ok := info.(map[string]any)["encrypted"]; ok {
		t.Fatal("info must carry no encryption claim")
	}

	// Chat list: only A's threads, newest message on top.
	chatSend(t, e, dmAID, bob, "text", "see you at the meetup", "")
	_, list := e.raw(t, "GET", "/communities/"+a+"/threads", aTok, nil)
	got := ids(list)
	if len(got) != 2 || got[0] != dmAID || got[1] != gid {
		t.Fatalf("list A: %v", got)
	}
	chatSend(t, e, gid, carol, "text", "hello crew", "")
	_, list = e.raw(t, "GET", "/communities/"+a+"/threads", aTok, nil)
	if got := ids(list); got[0] != gid {
		t.Fatalf("message should move group to top: %v", got)
	}
	_, listB := e.raw(t, "GET", "/communities/"+b+"/threads", aTok, nil)
	if got := ids(listB); len(got) != 1 || got[0] != field(dmB, "id") {
		t.Fatalf("list B: %v", got)
	}
	if code, _ := e.raw(t, "GET", "/communities/"+b+"/threads", cTok, nil); code != 403 {
		t.Fatalf("not joined list: %d", code)
	}

	// Filters and search.
	_, groups := e.raw(t, "GET", "/communities/"+a+"/threads?filter=groups", aTok, nil)
	if got := ids(groups); len(got) != 1 || got[0] != gid {
		t.Fatalf("groups filter: %v", got)
	}
	if err := e.td.As(alice, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `update public.thread_participants set last_read_at = now() where thread_id = $1 and user_id = auth.uid()`, dmAID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, unread := e.raw(t, "GET", "/communities/"+a+"/threads?filter=unread", aTok, nil)
	if got := ids(unread); len(got) != 1 || got[0] != gid {
		t.Fatalf("unread filter: %v", got)
	}
	_, search := e.raw(t, "GET", "/communities/"+a+"/threads?q=MEETUP", aTok, nil)
	if got := ids(search); len(got) != 1 || got[0] != dmAID {
		t.Fatalf("preview search: %v", got)
	}
	_, none := e.raw(t, "GET", "/communities/"+a+"/threads?q=xyz", aTok, nil)
	if l, ok := none.([]any); !ok || len(l) != 0 {
		t.Fatalf("no match: %v", none)
	}

	// Calls: a missed call appears under Calls.
	if code, out := e.raw(t, "POST", "/threads/"+dmAID+"/calls", aTok, map[string]any{"media": "audio", "outcome": "missed", "duration_seconds": 5}); code != 400 {
		t.Fatalf("missed with duration: %d %v", code, out)
	}
	if code, out := e.raw(t, "POST", "/threads/"+dmAID+"/calls", aTok, map[string]any{"media": "audio", "outcome": "missed"}); code != 201 || field(out, "outcome") != "missed" {
		t.Fatalf("log call: %d %v", code, out)
	}
	_, calls := e.raw(t, "GET", "/communities/"+a+"/threads?filter=calls", aTok, nil)
	if got := ids(calls); len(got) != 1 || got[0] != dmAID || field(field(calls.([]any)[0], "last_call"), "outcome") != "missed" {
		t.Fatalf("calls filter: %v", calls)
	}

	// Shared counts: PDF under documents, not media; deleting removes it.
	chatSend(t, e, dmAID, bob, "document", "spec.pdf", "https://x/spec.pdf")
	chatSend(t, e, dmAID, bob, "image", "", "https://x/a.png")
	_, info = e.raw(t, "GET", "/threads/"+dmAID, aTok, nil)
	sc := field(info, "shared_counts")
	if field(sc, "documents") != float64(1) || field(sc, "media") != float64(1) || field(field(info, "peer"), "id") != bob {
		t.Fatalf("shared counts: %v", info)
	}
	if _, err := e.td.Pool.Exec(context.Background(), `update public.messages set deleted_at = now() where kind = 'document'`); err != nil {
		t.Fatal(err)
	}
	_, info = e.raw(t, "GET", "/threads/"+dmAID, aTok, nil)
	if field(field(info, "shared_counts"), "documents") != float64(0) {
		t.Fatalf("deleted doc still counted: %v", info)
	}

	// Mute 8h: muted in list; expires by time alone.
	code, me := e.raw(t, "PATCH", "/threads/"+gid+"/me", aTok, map[string]any{"mute": "8h", "sound": false})
	if code != 200 || field(me, "muted_until") == nil || field(me, "sound") != false {
		t.Fatalf("mute: %d %v", code, me)
	}
	_, list = e.raw(t, "GET", "/communities/"+a+"/threads?filter=groups", aTok, nil)
	if field(list.([]any)[0], "muted") != true {
		t.Fatalf("muted flag: %v", list)
	}
	if code, _ := e.raw(t, "PATCH", "/threads/"+gid+"/me", aTok, map[string]any{"muted_until": "2000-01-01T00:00:00Z"}); code != 200 {
		t.Fatal("set past mute")
	}
	_, list = e.raw(t, "GET", "/communities/"+a+"/threads?filter=groups", aTok, nil)
	if field(list.([]any)[0], "muted") != false {
		t.Fatalf("expired mute: %v", list)
	}

	// Disappearing: earlier messages keep no expiry, later ones get 24h.
	if code, _ := e.raw(t, "PATCH", "/threads/"+dmAID+"/me?disappearing_seconds=3600", aTok, map[string]any{}); code != 400 {
		t.Fatalf("bad disappearing: %d", code)
	}
	if code, out := e.raw(t, "PATCH", "/threads/"+dmAID+"/me?disappearing_seconds=86400", aTok, map[string]any{}); code != 200 {
		t.Fatalf("disappearing: %d %v", code, out)
	}
	chatSend(t, e, dmAID, bob, "text", "ephemeral", "")
	var withExpiry, without int
	if err := e.td.Pool.QueryRow(context.Background(),
		`select count(*) filter (where expires_at = created_at + interval '24 hours'), count(*) filter (where expires_at is null) from public.messages where thread_id = $1`,
		dmAID).Scan(&withExpiry, &without); err != nil {
		t.Fatal(err)
	}
	if withExpiry != 1 || without != 3 {
		t.Fatalf("expiry: %d with, %d without", withExpiry, without)
	}

	// Clear: hidden for alice only; bob still sees the history.
	if code, _ := e.raw(t, "PATCH", "/threads/"+dmAID+"/me", aTok, map[string]any{"action": "clear"}); code != 200 {
		t.Fatal("clear")
	}
	count := func(uid, tid string) (n int) {
		_ = e.td.As(uid, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `select count(*) from public.messages where thread_id = $1`, tid).Scan(&n)
		})
		return n
	}
	if count(alice, dmAID) != 0 || count(bob, dmAID) == 0 {
		t.Fatalf("clear: alice %d bob %d", count(alice, dmAID), count(bob, dmAID))
	}

	// Exit group: carol gets no further messages and loses access.
	if code, _ := e.raw(t, "PATCH", "/threads/"+dmAID+"/me", aTok, map[string]any{"action": "exit"}); code != 400 {
		t.Fatal("exit a dm must be rejected")
	}
	if code, _ := e.raw(t, "PATCH", "/threads/"+gid+"/me", cTok, map[string]any{"action": "exit"}); code != 200 {
		t.Fatal("exit")
	}
	chatSend(t, e, gid, alice, "text", "after exit", "")
	if count(carol, gid) != 0 {
		t.Fatal("exited user still sees messages")
	}
	if code, _ := e.raw(t, "GET", "/threads/"+gid, cTok, nil); code != 403 {
		t.Fatalf("exited info: %d", code)
	}
	if code, _ := e.raw(t, "PATCH", "/threads/"+gid+"/me", cTok, map[string]any{"sound": true}); code != 403 {
		t.Fatal("exited settings")
	}

	// Delete chat hides it from the list until a new message arrives.
	e.raw(t, "PATCH", "/threads/"+dmAID+"/me", aTok, map[string]any{"action": "delete"})
	_, list = e.raw(t, "GET", "/communities/"+a+"/threads", aTok, nil)
	for _, id := range ids(list) {
		if id == dmAID {
			t.Fatal("deleted chat listed")
		}
	}

	// Blocked: DM not listed and cannot be reopened.
	chatSend(t, e, dmAID, bob, "text", "back", "")
	if _, err := e.td.Pool.Exec(context.Background(), `insert into public.blocks (blocker_id, blocked_id) values ($1, $2)`, bob, alice); err != nil {
		t.Fatal(err)
	}
	_, list = e.raw(t, "GET", "/communities/"+a+"/threads", aTok, nil)
	for _, id := range ids(list) {
		if id == dmAID {
			t.Fatal("blocked dm listed")
		}
	}
	if code, _ := e.raw(t, "POST", "/communities/"+a+"/threads/direct?user_id="+bob, aTok, nil); code != 403 {
		t.Fatalf("dm with block: %d", code)
	}
}
