package dbtest

import (
	"testing"
)

// newDM seeds a DM thread between a and b in a community (as service_role,
// standing in for the open-DM RPC) and returns its id.
func (d *DB) newDM(t *testing.T, community, a, b string) string {
	t.Helper()
	id := mustService[string](t, d, `insert into public.threads (community_id, kind, dm_key, created_by)
		values ($1, 'dm', public._dm_key($1, $2, $3), $2) returning id::text`, community, a, b)
	for _, u := range []string{a, b} {
		mustService[string](t, d, `insert into public.thread_participants (thread_id, user_id)
			values ($1, $2) returning id::text`, id, u)
	}
	return id
}

// send seeds a message (standing in for the send RPC) and returns its id.
func (d *DB) send(t *testing.T, thread, sender, body string) string {
	t.Helper()
	return mustService[string](t, d, `insert into public.messages (thread_id, sender_id, kind, body)
		values ($1, $2, 'text', $3) returning id::text`, thread, sender, body)
}

func (d *DB) su(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := d.Pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// chatFixture: Alice owns communities A and B; Bob is joined to both;
// Carol is joined to A only.
func chatFixture(t *testing.T) (d *DB, alice, bob, carol, a, b string) {
	d = New(t)
	alice = d.NewUser(t, "Alice A")
	bob = d.NewUser(t, "Bob B")
	carol = d.NewUser(t, "Carol C")
	a = d.createCommunity(t, alice, "Alpha", "private")
	b = d.createCommunity(t, alice, "Beta", "private")
	d.addMember(t, a, bob, "member")
	d.addMember(t, b, bob, "member")
	d.addMember(t, a, carol, "member")
	return
}

func TestThreadsAreScopedPerCommunity(t *testing.T) {
	d, alice, bob, carol, a, b := chatFixture(t)
	dmA := d.newDM(t, a, alice, bob)
	dmB := d.newDM(t, b, bob, alice)
	if dmA == dmB {
		t.Fatal("same thread across communities")
	}
	// The same pair in the same community is one thread (dm_key unique,
	// whichever side opens it).
	_, err := service[string](d, `insert into public.threads (community_id, kind, dm_key, created_by)
		values ($1, 'dm', public._dm_key($1, $2, $3), $2) returning id::text`, a, bob, alice)
	wantCode(t, err, "23505")
	if n := len(scalar[string](t, d, alice, `select dm_key from public.threads where id = $1`, dmA)); n != 110 {
		t.Fatalf("dm_key length %d", n)
	}

	// Switching scope: each community lists only its own threads.
	if id := scalar[string](t, d, alice, `select id::text from public.threads where community_id = $1`, b); id != dmB {
		t.Fatalf("B lists %s", id)
	}
	if n := scalar[int](t, d, bob, `select count(*)::int from public.threads where community_id = $1`, a); n != 1 {
		t.Fatalf("A lists %d threads for bob", n)
	}
	// Non-participants and visitors see nothing.
	if n := scalar[int](t, d, carol, `select count(*)::int from public.threads`); n != 0 {
		t.Fatalf("carol sees %d threads", n)
	}
	if n := scalar[int](t, d, "", `select count(*)::int from public.messages`); n != 0 {
		t.Fatalf("anon sees %d messages", n)
	}
	// Clients cannot create threads, participants or messages directly.
	wantCode(t, d.exec(alice, `insert into public.threads (community_id, kind, name, created_by)
		values ($1, 'group', 'g', auth.uid())`, a), "42501")
	wantCode(t, d.exec(carol, `insert into public.thread_participants (thread_id, user_id) values ($1, auth.uid())`, dmA), "42501")
	wantCode(t, d.exec(alice, `insert into public.messages (thread_id, sender_id, body) values ($1, auth.uid(), 'hi')`, dmA), "42501")

	// Leaving the community cuts access to its threads at once.
	d.su(t, `delete from public.memberships where community_id = $1 and user_id = $2`, a, bob)
	if n := scalar[int](t, d, bob, `select count(*)::int from public.threads`); n != 1 {
		t.Fatalf("bob still sees %d threads after leaving A", n)
	}
	if ok := mustService[bool](t, d, `select public.is_active_participant($1, $2)`, dmA, bob); ok {
		t.Fatal("bob still active in A's DM")
	}
}

func TestSendUnreadAndDeliveryState(t *testing.T) {
	d, alice, bob, carol, a, _ := chatFixture(t)
	older := d.newDM(t, a, alice, bob)
	newer := d.newDM(t, a, alice, carol)
	d.send(t, newer, carol, "first")
	d.send(t, older, bob, "  hello\n there  ")

	// A message moves its thread to the top of the list.
	top := scalar[string](t, d, alice, `select id::text from public.threads where community_id = $1
		order by coalesce(last_message_at, created_at) desc limit 1`, a)
	if top != older {
		t.Fatal("thread with the newest message is not on top")
	}
	if p := scalar[string](t, d, alice, `select last_message_preview from public.threads where id = $1`, older); p != "hello there" {
		t.Fatalf("preview %q", p)
	}

	// Sender's unread resets; recipient's counts up.
	m := d.send(t, older, alice, "reply")
	if n := scalar[int](t, d, alice, `select unread_count from public.thread_participants where thread_id = $1 and user_id = auth.uid()`, older); n != 0 {
		t.Fatalf("sender unread %d", n)
	}
	if n := scalar[int](t, d, bob, `select unread_count from public.thread_participants where thread_id = $1 and user_id = auth.uid()`, older); n != 1 {
		t.Fatalf("recipient unread %d", n)
	}
	// Not opened: never 'read'.
	if s := scalar[string](t, d, alice, `select public.message_status($1)`, m); s != "sent" {
		t.Fatalf("status %q, want sent", s)
	}
	if err := d.exec(bob, `update public.thread_participants set last_delivered_at = now()
		where thread_id = $1 and user_id = auth.uid()`, older); err != nil {
		t.Fatal(err)
	}
	if s := scalar[string](t, d, alice, `select public.message_status($1)`, m); s != "delivered" {
		t.Fatalf("status %q, want delivered", s)
	}
	// Opening the thread resets unread to 0 and makes it read.
	if err := d.exec(bob, `update public.thread_participants set last_read_at = now()
		where thread_id = $1 and user_id = auth.uid()`, older); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, d, bob, `select unread_count from public.thread_participants where thread_id = $1 and user_id = auth.uid()`, older); n != 0 {
		t.Fatalf("unread after open %d", n)
	}
	if s := scalar[string](t, d, alice, `select public.message_status($1)`, m); s != "read" {
		t.Fatalf("status %q, want read", s)
	}
	// Server-set columns and other people's rows stay out of reach.
	wantCode(t, d.exec(bob, `update public.thread_participants set unread_count = 5 where user_id = auth.uid()`), "42501")
	wantCode(t, d.exec(bob, `update public.thread_participants set left_at = now() where user_id = auth.uid()`), "42501")
	if n := scalar[int](t, d, bob, `with u as (update public.thread_participants set muted_until = now()
		where user_id <> auth.uid() returning 1) select count(*)::int from u`); n != 0 {
		t.Fatalf("bob updated %d foreign participant rows", n)
	}

	// Whitespace-only text is rejected.
	for _, body := range []string{"   ", "\n\t "} {
		_, err := service[string](d, `insert into public.messages (thread_id, sender_id, body) values ($1, $2, $3) returning id::text`, older, alice, body)
		wantCode(t, err, "23514")
	}
	_, err := service[string](d, `insert into public.messages (thread_id, sender_id, kind) values ($1, $2, 'text') returning id::text`, older, alice)
	wantCode(t, err, "23514")
	_, err = service[string](d, `insert into public.messages (thread_id, sender_id, kind, attachment_url, attachment_bytes)
		values ($1, $2, 'image', 'x', 26214401) returning id::text`, older, alice)
	wantCode(t, err, "23514")
}

func TestDeleteForEveryonePinsAndMarks(t *testing.T) {
	d, alice, bob, _, a, _ := chatFixture(t)
	th := d.newDM(t, a, alice, bob)
	first := d.send(t, th, bob, "first")
	m := mustService[string](t, d, `insert into public.messages (thread_id, sender_id, kind, body, attachment_url, attachment_bytes, reply_to_id)
		values ($1, $2, 'image', 'look', 'https://x/y.png', 10, $3) returning id::text`, th, alice, first)
	d.su(t, `update public.messages set pinned_at = now() where id = $1`, m)
	d.su(t, `insert into public.message_marks (message_id, user_id, kind) values ($1, $2, 'star')`, m, bob)
	d.su(t, `insert into public.message_marks (message_id, user_id, kind, emoji) values ($1, $2, 'reaction', '👍')`, m, bob)
	d.su(t, `insert into public.message_marks (message_id, user_id, kind) values ($1, $2, 'star')`, first, bob)

	// Stars are private to the starrer; reactions are shared.
	if n := scalar[int](t, d, alice, `select count(*)::int from public.message_marks where kind = 'star'`); n != 0 {
		t.Fatalf("alice sees %d of bob's stars", n)
	}
	if n := scalar[int](t, d, bob, `select count(*)::int from public.message_marks where kind = 'star'`); n != 2 {
		t.Fatalf("bob sees %d stars", n)
	}
	if n := scalar[int](t, d, alice, `select count(*)::int from public.message_marks where kind = 'reaction'`); n != 1 {
		t.Fatalf("alice sees %d reactions", n)
	}
	wantCode(t, d.exec(alice, `insert into public.message_marks (message_id, user_id, kind) values ($1, auth.uid(), 'star')`, m), "42501")

	pinned := `select count(*)::int from public.messages where thread_id = $1 and pinned_at is not null and deleted_at is null`
	if n := scalar[int](t, d, bob, pinned, th); n != 1 {
		t.Fatalf("pins %d", n)
	}

	// Delete for everyone: tombstone, pin gone, stars and reactions gone.
	d.su(t, `update public.messages set deleted_at = now() where id = $1`, m)
	var body, url, reply *string
	if err := d.Pool.QueryRow(ctx, `select body, attachment_url, reply_to_id::text from public.messages where id = $1`, m).Scan(&body, &url, &reply); err != nil {
		t.Fatal(err)
	}
	if body != nil || url != nil || reply != nil {
		t.Fatal("tombstone kept content")
	}
	if n := scalar[int](t, d, bob, `select count(*)::int from public.messages where id = $1 and deleted_at is not null`, m); n != 1 {
		t.Fatal("other participant does not see the tombstone")
	}
	if p := scalar[string](t, d, bob, `select last_message_preview from public.threads where id = $1`, th); p != "This message was deleted" {
		t.Fatalf("preview %q", p)
	}
	if n := scalar[int](t, d, bob, pinned, th); n != 0 {
		t.Fatal("deleted message still pinned")
	}
	if n := scalar[int](t, d, bob, `select count(*)::int from public.message_marks where message_id = $1`, m); n != 0 {
		t.Fatalf("%d marks survive delete for everyone", n)
	}
	if n := scalar[int](t, d, bob, `select count(*)::int from public.message_marks where message_id = $1`, first); n != 1 {
		t.Fatal("unrelated star removed")
	}
	// A tombstone cannot be revived.
	d.su(t, `update public.messages set deleted_at = null where id = $1`, m)
	if n := scalar[int](t, d, bob, `select count(*)::int from public.messages where id = $1 and deleted_at is not null`, m); n != 1 {
		t.Fatal("tombstone revived")
	}

	// Unpinning the last pin empties the banner.
	d.su(t, `update public.messages set pinned_at = now() where id = $1`, first)
	d.su(t, `update public.messages set pinned_at = null where id = $1`, first)
	if n := scalar[int](t, d, bob, pinned, th); n != 0 {
		t.Fatal("banner not empty")
	}

	// Delete for me hides the message for me only.
	d.su(t, `insert into public.message_marks (message_id, user_id, kind) values ($1, $2, 'hidden_for_me')`, first, alice)
	if n := scalar[int](t, d, alice, `select count(*)::int from public.messages where id = $1`, first); n != 0 {
		t.Fatal("hidden message still visible to me")
	}
	if n := scalar[int](t, d, bob, `select count(*)::int from public.messages where id = $1`, first); n != 1 {
		t.Fatal("delete for me hid it from others")
	}

	// Clear chat hides older messages for me only.
	if err := d.exec(bob, `update public.thread_participants set cleared_at = now() where thread_id = $1 and user_id = auth.uid()`, th); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, d, bob, `select count(*)::int from public.messages where thread_id = $1`, th); n != 0 {
		t.Fatalf("cleared chat shows %d", n)
	}
	if n := scalar[int](t, d, alice, `select count(*)::int from public.messages where thread_id = $1`, th); n != 1 {
		t.Fatalf("alice sees %d after bob cleared", n)
	}
}

func TestDisappearingAndCallLogs(t *testing.T) {
	d, alice, bob, _, a, _ := chatFixture(t)
	th := d.newDM(t, a, alice, bob)
	d.su(t, `update public.threads set disappearing_seconds = 86400 where id = $1`, th)
	m := d.send(t, th, alice, "brief")
	if ok := scalar[bool](t, d, bob, `select expires_at = created_at + interval '1 day' from public.messages where id = $1`, m); !ok {
		t.Fatal("expires_at not stamped")
	}
	d.su(t, `update public.messages set expires_at = now() - interval '1 second' where id = $1`, m)
	if n := scalar[int](t, d, bob, `select count(*)::int from public.messages where id = $1`, m); n != 0 {
		t.Fatal("expired message visible")
	}
	_, err := service[string](d, `update public.threads set disappearing_seconds = 60 where id = $1 returning id::text`, th)
	wantCode(t, err, "23514")

	// An unanswered call is logged as missed and shows in the Calls tab.
	mustService[string](t, d, `insert into public.call_logs (thread_id, caller_id, media, outcome)
		values ($1, $2, 'video', 'missed') returning id::text`, th, alice)
	if o := scalar[string](t, d, bob, `select outcome from public.call_logs where thread_id = $1`, th); o != "missed" {
		t.Fatalf("outcome %q", o)
	}
	_, err = service[string](d, `insert into public.call_logs (thread_id, caller_id, media, outcome, duration_seconds)
		values ($1, $2, 'audio', 'missed', 5) returning id::text`, th, alice)
	wantCode(t, err, "23514")
}

func TestChatTablesArePublishedToRealtime(t *testing.T) {
	d := New(t)
	var n int
	if err := d.Pool.QueryRow(ctx, `select count(*)::int from pg_publication_tables
		where pubname = 'supabase_realtime' and schemaname = 'public'
		  and tablename in ('threads', 'thread_participants', 'messages', 'message_marks', 'call_logs')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("%d chat tables in supabase_realtime, want 5", n)
	}
}
