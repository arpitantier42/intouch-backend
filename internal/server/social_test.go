package server

import (
	"context"
	"testing"
)

func items(v any) []any {
	l, _ := field(v, "items").([]any)
	return l
}

func count(v any, tab string) float64 {
	n, _ := field(v, "counts").(map[string]any)[tab].(float64)
	return n
}

func connState(t *testing.T, e *sessEnv, me, other string) string {
	t.Helper()
	var s string
	if err := e.td.Pool.QueryRow(context.Background(),
		`select set_config('request.jwt.claims', json_build_object('sub', $1::text, 'role', 'authenticated')::text, true),
		        public.connection_state($2)`, me, other).Scan(new(string), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConnectionsFlow(t *testing.T) {
	e, _ := newMsgEnv(t)
	alice, aTok := admUser(t, e, "alice@example.com")
	bob, bTok := admUser(t, e, "bob@example.com")
	carol, cTok := admUser(t, e, "carol@example.com")
	dave, dTok := admUser(t, e, "dave@example.com")

	// Validation.
	if code, _ := e.raw(t, "POST", "/connections", aTok, map[string]any{}); code != 400 {
		t.Fatalf("missing addressee: %d", code)
	}
	if code, _ := e.raw(t, "POST", "/connections", aTok, map[string]any{"addressee_id": alice}); code != 400 {
		t.Fatalf("self: %d", code)
	}
	if code, _ := e.raw(t, "GET", "/me/connections", aTok, nil); code != 400 {
		t.Fatalf("missing tab: %d", code)
	}

	// Alice requests Bob: Bob sees it under Incoming.
	code, req := e.raw(t, "POST", "/connections", aTok, map[string]any{"addressee_id": bob})
	if code != 201 || field(req, "state") != "pending" || field(req, "requester_id") != alice {
		t.Fatalf("send: %d %v", code, req)
	}
	rid := field(req, "id").(string)
	if code, _ := e.raw(t, "POST", "/connections", aTok, map[string]any{"addressee_id": bob}); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	if code, _ := e.raw(t, "POST", "/connections", bTok, map[string]any{"user_id": alice}); code != 409 {
		t.Fatalf("reverse duplicate: %d", code)
	}
	_, inc := e.raw(t, "GET", "/me/connections?tab=incoming", bTok, nil)
	if l := items(inc); len(l) != 1 || field(l[0], "id") != rid || field(field(l[0], "peer"), "id") != alice {
		t.Fatalf("bob incoming: %v", inc)
	}

	// Three incoming requests → count 3.
	e.raw(t, "POST", "/connections", cTok, map[string]any{"addressee_id": bob})
	e.raw(t, "POST", "/connections", dTok, map[string]any{"addressee_id": bob})
	_, inc = e.raw(t, "GET", "/me/connections?tab=incoming", bTok, nil)
	if count(inc, "incoming") != 3 || len(items(inc)) != 3 {
		t.Fatalf("incoming count: %v", inc)
	}

	// Only the addressee responds.
	if code, _ := e.raw(t, "PATCH", "/connections/"+rid+"?action=accept", aTok, nil); code != 403 {
		t.Fatalf("requester accept: %d", code)
	}
	if code, _ := e.raw(t, "PATCH", "/connections/"+rid+"?action=accept", cTok, nil); code != 404 {
		t.Fatalf("stranger accept: %d", code)
	}
	if code, _ := e.raw(t, "PATCH", "/connections/"+rid+"?action=maybe", bTok, nil); code != 400 {
		t.Fatalf("bad action: %d", code)
	}

	// Decline is silent; Undo returns it to Incoming.
	if code, out := e.raw(t, "PATCH", "/connections/"+rid+"?action=decline", bTok, nil); code != 200 || field(out, "state") != "declined" {
		t.Fatalf("decline: %d %v", code, out)
	}
	_, sent := e.raw(t, "GET", "/me/connections?tab=sent", aTok, nil)
	if l := items(sent); len(l) != 1 || field(l[0], "state") != "pending" {
		t.Fatalf("silent decline leaked: %v", sent)
	}
	_, inc = e.raw(t, "GET", "/me/connections?tab=incoming", bTok, nil)
	if count(inc, "incoming") != 2 {
		t.Fatalf("declined still incoming: %v", inc)
	}
	if code, out := e.raw(t, "PATCH", "/connections/"+rid, bTok, map[string]any{"action": "undo"}); code != 200 || field(out, "state") != "pending" {
		t.Fatalf("undo: %d %v", code, out)
	}
	_, inc = e.raw(t, "GET", "/me/connections?tab=incoming", bTok, nil)
	if count(inc, "incoming") != 3 {
		t.Fatalf("undo not incoming: %v", inc)
	}

	// Accept → Connected for both.
	if code, out := e.raw(t, "PATCH", "/connections/"+rid+"?action=accept", bTok, nil); code != 200 || field(out, "state") != "connected" {
		t.Fatalf("accept: %d %v", code, out)
	}
	_, con := e.raw(t, "GET", "/me/connections?tab=connected", aTok, nil)
	if count(con, "connected") != 1 || connState(t, e, alice, bob) != "connected" {
		t.Fatalf("connected: %v", con)
	}

	// Remove → Idle; Connect available again.
	if code, _ := e.raw(t, "DELETE", "/connections/"+rid, cTok, nil); code != 404 {
		t.Fatalf("stranger delete: %d", code)
	}
	if code, _ := e.raw(t, "DELETE", "/connections/"+rid, aTok, nil); code != 204 {
		t.Fatalf("remove: %d", code)
	}
	if s := connState(t, e, bob, alice); s != "idle" {
		t.Fatalf("after remove: %s", s)
	}
	var notes int
	_ = e.td.Pool.QueryRow(context.Background(), `select count(*) from public.notifications where user_id = $1`, bob).Scan(&notes)
	if notes != 0 {
		t.Fatalf("removal notified: %d", notes)
	}
	code, req = e.raw(t, "POST", "/connections", aTok, map[string]any{"addressee_id": bob})
	if code != 201 {
		t.Fatalf("reconnect: %d", code)
	}

	// Withdraw (tap Requested) → Idle.
	if code, _ := e.raw(t, "DELETE", "/connections/"+field(req, "id").(string), aTok, nil); code != 204 {
		t.Fatalf("withdraw: %d", code)
	}
	if s := connState(t, e, alice, bob); s != "idle" {
		t.Fatalf("after withdraw: %s", s)
	}
	_ = carol
	_ = dave

	// Connection changes are broadcast through Realtime.
	var published bool
	_ = e.td.Pool.QueryRow(context.Background(), `select exists (select 1 from pg_publication_tables
		where pubname = 'supabase_realtime' and tablename = 'connections')`).Scan(&published)
	if !published {
		t.Fatal("connections not in supabase_realtime")
	}
}

func TestBlocksFlow(t *testing.T) {
	e, _ := newMsgEnv(t)
	alice, aTok := admUser(t, e, "alice@example.com")
	bob, bTok := admUser(t, e, "bob@example.com")
	cid, _ := admCommunity(t, e, alice, "Alpha", "private")
	chatJoin(t, e, cid, bob)

	_, dm := e.raw(t, "POST", "/communities/"+cid+"/threads/direct?user_id="+bob, aTok, nil)
	tid := field(dm, "id").(string)
	if code, out := send(t, e, tid, cid, aTok, text("hello")); code != 201 {
		t.Fatalf("send before block: %d %v", code, out)
	}

	// Connected pair, then Bob blocks Alice.
	_, req := e.raw(t, "POST", "/connections", aTok, map[string]any{"addressee_id": bob})
	e.raw(t, "PATCH", "/connections/"+field(req, "id").(string)+"?action=accept", bTok, nil)

	if code, _ := e.raw(t, "POST", "/me/blocks", bTok, map[string]any{"blocked_id": bob}); code != 400 {
		t.Fatalf("block self: %d", code)
	}
	if code, _ := e.raw(t, "POST", "/me/blocks", bTok, map[string]any{}); code != 400 {
		t.Fatalf("block nobody: %d", code)
	}
	code, blk := e.raw(t, "POST", "/me/blocks", bTok, map[string]any{"blocked_id": alice})
	if code != 201 || field(blk, "blocker_id") != bob || field(blk, "blocked_id") != alice {
		t.Fatalf("block: %d %v", code, blk)
	}
	if code, _ := e.raw(t, "POST", "/me/blocks", bTok, map[string]any{"user_id": alice}); code != 201 {
		t.Fatalf("block again: %d", code)
	}

	// Connection gone; neither side can message or reconnect.
	_, con := e.raw(t, "GET", "/me/connections?tab=connected", aTok, nil)
	if count(con, "connected") != 0 {
		t.Fatalf("connection survived block: %v", con)
	}
	if code, _ := send(t, e, tid, cid, aTok, text("hi")); code != 403 {
		t.Fatalf("alice sends to blocker: %d", code)
	}
	if code, _ := send(t, e, tid, cid, bTok, text("hi")); code != 403 {
		t.Fatalf("blocker sends: %d", code)
	}
	if code, _ := e.raw(t, "POST", "/connections", aTok, map[string]any{"addressee_id": bob}); code != 403 {
		t.Fatalf("connect while blocked: %d", code)
	}

	// Settings → Blocked accounts; Alice cannot see Bob's block.
	_, list := e.raw(t, "GET", "/me/blocks", bTok, nil)
	l, _ := list.([]any)
	if len(l) != 1 || field(field(l[0], "profile"), "id") != alice {
		t.Fatalf("list blocks: %v", list)
	}
	_, list = e.raw(t, "GET", "/me/blocks", aTok, nil)
	if l, _ := list.([]any); len(l) != 0 {
		t.Fatalf("blocked user sees block: %v", list)
	}

	// Alice cannot unblock herself out of Bob's block (no-op).
	e.raw(t, "DELETE", "/me/blocks/"+bob, aTok, nil)
	if code, _ := send(t, e, tid, cid, aTok, text("hi")); code != 403 {
		t.Fatalf("still blocked: %d", code)
	}

	// Unblock → Idle (not Connected); sending works again.
	if code, _ := e.raw(t, "DELETE", "/me/blocks/"+alice, bTok, nil); code != 204 {
		t.Fatalf("unblock: %d", code)
	}
	if s := connState(t, e, bob, alice); s != "idle" {
		t.Fatalf("after unblock: %s", s)
	}
	if code, out := send(t, e, tid, cid, aTok, text("hi again")); code != 201 {
		t.Fatalf("send after unblock: %d %v", code, out)
	}
	if code, _ := e.raw(t, "DELETE", "/me/blocks/not-a-uuid", bTok, nil); code != 400 {
		t.Fatalf("bad uuid: %d", code)
	}
}
