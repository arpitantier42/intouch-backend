package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/intouch/api/internal/config"
	"github.com/intouch/api/internal/db"
	"github.com/intouch/api/internal/push"
)

type fakePush struct {
	mu   sync.Mutex
	sent []push.Notification
}

func (f *fakePush) Send(_ context.Context, n push.Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, n)
	return nil
}

// last waits briefly for the background push and returns the latest one.
func (f *fakePush) last(t *testing.T, want int) push.Notification {
	t.Helper()
	for i := 0; i < 100; i++ {
		f.mu.Lock()
		if len(f.sent) >= want {
			n := f.sent[len(f.sent)-1]
			f.mu.Unlock()
			return n
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d pushes", want)
	return push.Notification{}
}

func (f *fakePush) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func newMsgEnv(t *testing.T) (*sessEnv, *fakePush) {
	e := newSessEnv(t)
	fp := &fakePush{}
	e.r = New(Deps{Config: config.Config{JWTSecret: secret}, DB: &db.DB{Pool: e.td.Pool}, Auth: e.fake, Push: fp})
	return e, fp
}

// send posts a message with the scope header.
func send(t *testing.T, e *sessEnv, tid, scope, token string, body any) (int, any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/threads/"+tid+"/messages", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if scope != "" {
		req.Header.Set("scope_community_id", scope)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	var out any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func text(s string) map[string]any { return map[string]any{"kind": "text", "body": s} }

// threadOf finds a thread in a listThreads result.
func threadOf(t *testing.T, e *sessEnv, cid, tid, token string) (pos int, row any) {
	t.Helper()
	_, list := e.raw(t, "GET", "/communities/"+cid+"/threads", token, nil)
	for i, x := range list.([]any) {
		if field(x, "id") == tid {
			return i, x
		}
	}
	return -1, nil
}

func TestMessaging(t *testing.T) {
	e, fp := newMsgEnv(t)
	alice, aTok := admUser(t, e, "alice@example.com")
	bob, bTok := admUser(t, e, "bob@example.com")
	carol, cTok := admUser(t, e, "carol@example.com")
	a, _ := admCommunity(t, e, alice, "Alpha", "private")
	b, _ := admCommunity(t, e, alice, "Beta", "private")
	chatJoin(t, e, a, bob)
	chatJoin(t, e, b, bob)
	chatJoin(t, e, a, carol)

	_, dm := e.raw(t, "POST", "/communities/"+a+"/threads/direct?user_id="+bob, aTok, nil)
	dmID := field(dm, "id").(string)

	// Whitespace-only text is not sent (400); missing scope 400.
	if code, out := send(t, e, dmID, a, aTok, text("   \n ")); code != 400 {
		t.Fatalf("blank: %d %v", code, out)
	}
	if code, _ := send(t, e, dmID, "", aTok, text("hi")); code != 400 {
		t.Fatalf("no scope: %d", code)
	}
	// Scoped to B while sending to an A thread: rejected.
	if code, out := send(t, e, dmID, b, aTok, text("hi")); code != 403 {
		t.Fatalf("out of scope: %d %v", code, out)
	}
	// Attachment over 25 MB → 413.
	if code, _ := send(t, e, dmID, a, aTok, map[string]any{"kind": "document", "attachment_url": "x/y.pdf", "attachment_bytes": 26214401}); code != 413 {
		t.Fatalf("too large: %d", code)
	}
	// Non-participant → 403.
	if code, _ := send(t, e, dmID, a, cTok, text("hi")); code != 403 {
		t.Fatalf("non participant: %d", code)
	}

	// Three messages from alice; bob has 3 unread; state is 'sent' (not read).
	var m1 any
	for i, s := range []string{"one", "two", "three"} {
		code, out := send(t, e, dmID, a, aTok, text(s))
		if code != 201 || field(out, "status") != "sent" || field(out, "body") != s {
			t.Fatalf("send %d: %d %v", i, code, out)
		}
		if i == 0 {
			m1 = out
		}
	}
	m1ID := field(m1, "id").(string)
	n := fp.last(t, 1)
	if len(n.UserIDs) != 1 || n.UserIDs[0] != bob || n.Data["thread_id"] != dmID {
		t.Fatalf("push: %+v", n)
	}
	if _, row := threadOf(t, e, a, dmID, bTok); field(row, "unread_count") != float64(3) {
		t.Fatalf("bob unread: %v", row)
	}

	// Bob replies without opening: his unread resets to 0 (AC26), thread on top.
	code, rep := send(t, e, dmID, a, bTok, map[string]any{"kind": "text", "body": "hey", "reply_to_id": m1ID})
	if code != 201 || field(rep, "reply_to_id") != m1ID {
		t.Fatalf("reply: %d %v", code, rep)
	}
	if _, row := threadOf(t, e, a, dmID, bTok); field(row, "unread_count") != float64(0) {
		t.Fatalf("after reply unread: %v", row)
	}

	// 5 threads: sending in the oldest moves it first.
	var groups []string
	for _, name := range []string{"g1", "g2", "g3", "g4"} {
		_, g := e.raw(t, "POST", "/communities/"+a+"/threads/groups?member_ids="+bob+","+carol, aTok, map[string]string{"name": name})
		gid := field(g, "id").(string)
		groups = append(groups, gid)
		send(t, e, gid, a, aTok, text("hello "+name))
	}
	if pos, _ := threadOf(t, e, a, dmID, aTok); pos != 4 {
		t.Fatalf("dm should be 5th, is %d", pos)
	}
	send(t, e, dmID, a, aTok, text("back to top"))
	if pos, _ := threadOf(t, e, a, dmID, aTok); pos != 0 {
		t.Fatalf("dm should be first, is %d", pos)
	}

	// Opening the thread: list + unread reset; alice's messages become read.
	code, list := e.raw(t, "GET", "/threads/"+dmID+"/messages", bTok, nil)
	if code != 200 || len(list.([]any)) != 5 {
		t.Fatalf("list: %d %v", code, list)
	}
	if field(list.([]any)[0], "can_edit") != false {
		t.Fatal("edit offered on someone else's message")
	}
	if _, row := threadOf(t, e, a, dmID, bTok); field(row, "unread_count") != float64(0) {
		t.Fatalf("open unread: %v", row)
	}
	_, mine := e.raw(t, "GET", "/threads/"+dmID+"/messages", aTok, nil)
	if field(mine.([]any)[0], "status") != "read" || field(mine.([]any)[0], "can_edit") != true {
		t.Fatalf("status after open: %v", mine.([]any)[0])
	}
	// Catch-up after reconnect: only newer messages.
	after := field(m1, "created_at").(string)
	_, newer := e.raw(t, "GET", "/threads/"+dmID+"/messages?after="+url.QueryEscape(after), bTok, nil)
	if l, ok := newer.([]any); !ok || len(l) != 4 {
		t.Fatalf("catch-up: %v", newer)
	}
	// carol is not a participant.
	if code, _ := e.raw(t, "GET", "/threads/"+dmID+"/messages", cTok, nil); code != 403 {
		t.Fatalf("list non participant: %d", code)
	}

	// Edit: non-author 403; blank keeps text; edit sets edited_at.
	if code, _ := e.raw(t, "PATCH", "/messages/"+m1ID, bTok, map[string]string{"body": "hacked"}); code != 403 {
		t.Fatalf("non-author edit: %d", code)
	}
	code, ed := e.raw(t, "PATCH", "/messages/"+m1ID, aTok, map[string]string{"body": "  "})
	if code != 200 || field(ed, "body") != "one" || field(ed, "edited_at") != nil {
		t.Fatalf("blank edit: %d %v", code, ed)
	}
	code, ed = e.raw(t, "PATCH", "/messages/"+m1ID, aTok, map[string]string{"body": "one!"})
	if code != 200 || field(ed, "body") != "one!" || field(ed, "edited_at") == nil {
		t.Fatalf("edit: %d %v", code, ed)
	}

	// Marks: bob stars; alice does not see it in her starred view.
	if code, out := e.raw(t, "PUT", "/messages/"+m1ID+"/marks?action=star", bTok, nil); code != 200 || field(out, "kind") != "star" {
		t.Fatalf("star: %d %v", code, out)
	}
	_, st := e.raw(t, "GET", "/threads/"+dmID+"/messages?view=starred", aTok, nil)
	_, stB := e.raw(t, "GET", "/threads/"+dmID+"/messages?view=starred", bTok, nil)
	if len(st.([]any)) != 0 || len(stB.([]any)) != 1 {
		t.Fatalf("starred: alice %v bob %v", st, stB)
	}
	if code, out := e.raw(t, "PUT", "/messages/"+m1ID+"/marks?action=react", aTok, map[string]string{"emoji": "👍"}); code != 200 || field(out, "emoji") != "👍" {
		t.Fatalf("react: %d %v", code, out)
	}
	if code, _ := e.raw(t, "PUT", "/messages/"+m1ID+"/marks?action=react", aTok, nil); code != 400 {
		t.Fatalf("react without emoji: %d", code)
	}
	// Pin, unpin → banner empty; pin again then delete → banner empty.
	e.raw(t, "PUT", "/messages/"+m1ID+"/marks?action=pin", bTok, nil)
	_, pins := e.raw(t, "GET", "/threads/"+dmID+"/messages?view=pinned", aTok, nil)
	if len(pins.([]any)) != 1 {
		t.Fatalf("pinned: %v", pins)
	}
	e.raw(t, "PUT", "/messages/"+m1ID+"/marks?action=unpin", aTok, nil)
	if _, pins = e.raw(t, "GET", "/threads/"+dmID+"/messages?view=pinned", aTok, nil); len(pins.([]any)) != 0 {
		t.Fatalf("after unpin: %v", pins)
	}
	e.raw(t, "PUT", "/messages/"+m1ID+"/marks?action=pin", aTok, nil)

	// Search in conversation.
	if _, found := e.raw(t, "GET", "/threads/"+dmID+"/messages?q=thr", aTok, nil); len(found.([]any)) != 1 {
		t.Fatalf("search: %v", found)
	}

	// Delete: for me hides only for me; for everyone needs the author.
	m2ID := field(mine.([]any)[1], "id").(string)
	if code, _ := e.raw(t, "DELETE", "/messages/"+m2ID+"?scope=me", bTok, nil); code != 204 {
		t.Fatalf("delete for me: %d", code)
	}
	_, bl := e.raw(t, "GET", "/threads/"+dmID+"/messages", bTok, nil)
	_, al := e.raw(t, "GET", "/threads/"+dmID+"/messages", aTok, nil)
	if len(bl.([]any)) != len(al.([]any))-1 {
		t.Fatalf("delete for me: bob %d alice %d", len(bl.([]any)), len(al.([]any)))
	}
	if code, _ := e.raw(t, "DELETE", "/messages/"+m1ID+"?scope=everyone", bTok, nil); code != 403 {
		t.Fatalf("non-author delete: %d", code)
	}
	if code, _ := e.raw(t, "DELETE", "/messages/"+m1ID+"?scope=everyone", aTok, nil); code != 204 {
		t.Fatalf("delete for everyone: %d", code)
	}
	_, bl = e.raw(t, "GET", "/threads/"+dmID+"/messages", bTok, nil)
	tomb := bl.([]any)[0]
	if field(tomb, "deleted_at") == nil || field(tomb, "body") != nil || field(tomb, "pinned_at") != nil ||
		len(field(tomb, "reactions").([]any)) != 0 || field(tomb, "starred") != false {
		t.Fatalf("tombstone: %v", tomb)
	}
	if _, pins = e.raw(t, "GET", "/threads/"+dmID+"/messages?view=pinned", bTok, nil); len(pins.([]any)) != 0 {
		t.Fatalf("pin after delete: %v", pins)
	}
	if code, _ := e.raw(t, "DELETE", "/messages/"+m1ID+"?scope=everyone", aTok, nil); code != 409 {
		t.Fatalf("delete twice: %d", code)
	}
	if code, _ := e.raw(t, "PATCH", "/messages/"+m1ID, aTok, map[string]string{"body": "x"}); code != 409 {
		t.Fatalf("edit deleted: %d", code)
	}
	if code, _ := e.raw(t, "PUT", "/messages/"+m1ID+"/marks?action=star", aTok, nil); code != 409 {
		t.Fatalf("mark deleted: %d", code)
	}

	// Forward to three chats: each gets a copy marked forwarded.
	src := field(mine.([]any)[2], "id").(string)
	for _, gid := range groups[:3] {
		code, out := send(t, e, gid, a, aTok, map[string]any{"forward_of_id": src})
		if code != 201 || field(out, "forwarded") != true || field(out, "body") != "three" {
			t.Fatalf("forward: %d %v", code, out)
		}
	}

	// Mute: muted group members get no push; an expired mute alerts again.
	gid := groups[3]
	e.raw(t, "PATCH", "/threads/"+gid+"/me", bTok, map[string]string{"mute": "8h"})
	before := fp.count()
	send(t, e, gid, a, aTok, text("ping"))
	n = fp.last(t, before+1)
	if len(n.UserIDs) != 1 || n.UserIDs[0] != carol {
		t.Fatalf("muted push: %+v", n)
	}
	if _, err := e.td.Pool.Exec(context.Background(),
		`update public.thread_participants set muted_until = now() - interval '1 minute' where thread_id = $1 and user_id = $2`, gid, bob); err != nil {
		t.Fatal(err)
	}
	send(t, e, gid, a, aTok, text("ping again"))
	if n = fp.last(t, before+2); len(n.UserIDs) != 2 {
		t.Fatalf("mute expired push: %+v", n)
	}

	// Blocks: either direction rejects the DM send; unblock restores it.
	if _, err := e.td.Pool.Exec(context.Background(),
		`insert into public.blocks (blocker_id, blocked_id) values ($1, $2)`, bob, alice); err != nil {
		t.Fatal(err)
	}
	if code, _ := send(t, e, dmID, a, aTok, text("blocked?")); code != 403 {
		t.Fatalf("recipient blocked sender: %d", code)
	}
	if code, _ := send(t, e, dmID, a, bTok, text("blocked?")); code != 403 {
		t.Fatalf("blocker sends: %d", code)
	}
	e.td.Pool.Exec(context.Background(), `delete from public.blocks where blocker_id = $1`, bob)
	if code, _ := send(t, e, dmID, a, aTok, text("unblocked")); code != 201 {
		t.Fatalf("after unblock: %d", code)
	}

	// DM peer who left the community: send rejected.
	e.td.Pool.Exec(context.Background(), `delete from public.memberships where community_id = $1 and user_id = $2`, a, bob)
	if code, _ := send(t, e, dmID, a, aTok, text("still there?")); code != 403 {
		t.Fatalf("peer not joined: %d", code)
	}
}
