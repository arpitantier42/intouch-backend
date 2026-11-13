package dbtest

import (
	"testing"
)

func (d *DB) state(t *testing.T, as, other string) string {
	t.Helper()
	return scalar[string](t, d, as, `select public.connection_state($1)`, other)
}

func TestConnectionLifecycle(t *testing.T) {
	d := New(t)
	alice := d.NewUser(t, "Alice A")
	bob := d.NewUser(t, "Bob B")

	if s := d.state(t, alice, bob); s != "idle" {
		t.Fatalf("initial state %q", s)
	}
	// Alice requests Bob: Sent for Alice, Incoming for Bob.
	id := scalar[string](t, d, alice, `select id::text from public.send_connection_request($1)`, bob)
	if s := d.state(t, alice, bob); s != "sent" {
		t.Fatalf("alice state %q", s)
	}
	if s := d.state(t, bob, alice); s != "incoming" {
		t.Fatalf("bob state %q", s)
	}
	incoming := scalar[int](t, d, bob, `select count(*)::int from public.connections
		where addressee_id = auth.uid() and state = 'pending' and requester_id = $1`, alice)
	if incoming != 1 {
		t.Fatalf("bob incoming = %d", incoming)
	}
	key := scalar[string](t, d, bob, `select pair_key from public.connections where id = $1`, id)
	if len(key) != 73 {
		t.Fatalf("pair_key %q", key)
	}

	// One row per pair: neither side can send again while pending.
	wantCode(t, d.exec(alice, `select public.send_connection_request($1)`, bob), "PT409")
	wantCode(t, d.exec(bob, `select public.send_connection_request($1)`, alice), "PT409")
	wantCode(t, d.exec(alice, `select public.send_connection_request(auth.uid())`), "PT400")
	// Only the addressee responds; a stranger cannot even see the row.
	wantCode(t, d.exec(alice, `select public.respond_to_connection($1, 'accept')`, id), "PT403")
	carol := d.NewUser(t, "Carol C")
	wantCode(t, d.exec(carol, `select public.respond_to_connection($1, 'accept')`, id), "PT404")
	if n := scalar[int](t, d, carol, `select count(*)::int from public.connections`); n != 0 {
		t.Fatalf("stranger sees %d connections", n)
	}
	// Clients cannot insert or change state directly.
	wantCode(t, d.exec(alice, `insert into public.connections (requester_id, addressee_id) values (auth.uid(), $1)`, carol), "42501")
	wantCode(t, d.exec(bob, `update public.connections set state = 'connected' where id = $1`, id), "42501")

	// Ignore, then Undo, then Accept.
	if s := scalar[string](t, d, bob, `select state from public.respond_to_connection($1, 'decline')`, id); s != "declined" {
		t.Fatalf("decline -> %q", s)
	}
	if s := d.state(t, alice, bob); s != "sent" {
		t.Fatalf("declined looks %q to the requester, want sent (silent)", s)
	}
	wantCode(t, d.exec(bob, `select public.respond_to_connection($1, 'decline')`, id), "PT409")
	if s := scalar[string](t, d, bob, `select state from public.respond_to_connection($1, 'undo')`, id); s != "pending" {
		t.Fatalf("undo -> %q", s)
	}
	if s := scalar[string](t, d, bob, `select state from public.respond_to_connection($1, 'accept')`, id); s != "connected" {
		t.Fatalf("accept -> %q", s)
	}
	if d.state(t, alice, bob) != "connected" || d.state(t, bob, alice) != "connected" {
		t.Fatal("not connected both ways")
	}
	wantCode(t, d.exec(bob, `select public.respond_to_connection($1, 'nope')`, id), "PT400")

	// Remove: either party deletes the row; back to Idle.
	if err := d.exec(bob, `delete from public.connections where id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if s := d.state(t, alice, bob); s != "idle" {
		t.Fatalf("after remove %q", s)
	}
}

func TestWithdrawReturnsToIdle(t *testing.T) {
	d := New(t)
	alice := d.NewUser(t, "Alice A")
	bob := d.NewUser(t, "Bob B")
	id := scalar[string](t, d, alice, `select id::text from public.send_connection_request($1)`, bob)
	if err := d.exec(alice, `delete from public.connections where id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if d.state(t, alice, bob) != "idle" || d.state(t, bob, alice) != "idle" {
		t.Fatal("withdraw did not return to idle")
	}
	// A request the caller had declined is replaced by the caller's own.
	id = scalar[string](t, d, alice, `select id::text from public.send_connection_request($1)`, bob)
	if err := d.exec(bob, `select public.respond_to_connection($1, 'decline')`, id); err != nil {
		t.Fatal(err)
	}
	wantCode(t, d.exec(alice, `select public.send_connection_request($1)`, bob), "PT409")
	if err := d.exec(bob, `select public.send_connection_request($1)`, alice); err != nil {
		t.Fatalf("re-request after own decline: %v", err)
	}
	if s := d.state(t, alice, bob); s != "incoming" {
		t.Fatalf("alice now sees %q", s)
	}
}

func TestBlockingResetsRelationship(t *testing.T) {
	d := New(t)
	alice := d.NewUser(t, "Alice A")
	bob := d.NewUser(t, "Bob B")
	id := scalar[string](t, d, alice, `select id::text from public.send_connection_request($1)`, bob)
	if err := d.exec(bob, `select public.respond_to_connection($1, 'accept')`, id); err != nil {
		t.Fatal(err)
	}
	if err := d.exec(alice, `select public.follow_user($1)`, bob); err != nil {
		t.Fatal(err)
	}
	if err := d.exec(bob, `select public.follow_user($1)`, alice); err != nil {
		t.Fatal(err)
	}

	// Bob blocks Alice: connection and follows are gone.
	blockID := scalar[string](t, d, bob, `select id::text from public.block_user($1)`, alice)
	again := scalar[string](t, d, bob, `select id::text from public.block_user($1)`, alice)
	if again != blockID {
		t.Fatal("blocking twice created a second row")
	}
	var n int
	_ = d.Pool.QueryRow(ctx, `select (select count(*) from public.connections) + (select count(*) from public.follows)`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d connection/follow rows survive the block", n)
	}
	if d.state(t, bob, alice) != "blocked" {
		t.Fatal("blocker should see blocked")
	}
	if d.state(t, alice, bob) != "idle" {
		t.Fatal("blocked user must not learn of the block")
	}
	// The block is visible both ways to the filters chat/feed/people use.
	for _, u := range []string{alice, bob} {
		other := map[string]string{alice: bob, bob: alice}[u]
		if !scalar[bool](t, d, u, `select public.is_blocked_with($1)`, other) {
			t.Fatal("is_blocked_with false")
		}
		if !scalar[bool](t, d, u, `select $1::uuid in (select public.blocked_user_ids())`, other) {
			t.Fatal("blocked_user_ids misses the other user")
		}
	}
	// Alice can neither connect, follow nor request mentorship from Bob.
	wantCode(t, d.exec(alice, `select public.send_connection_request($1)`, bob), "PT403")
	wantCode(t, d.exec(alice, `select public.follow_user($1)`, bob), "PT403")
	wantCode(t, d.exec(alice, `select public.request_mentorship($1, 'career', 'hi')`, bob), "PT404")
	wantCode(t, d.exec(bob, `select public.block_user(auth.uid())`), "PT400")
	// blocked_between is for the service only.
	wantCode(t, d.exec(alice, `select public.blocked_between($1, $2)`, alice, bob), "42501")

	// Settings → Blocked accounts: Bob lists Alice; Alice sees nothing.
	if s := scalar[string](t, d, bob, `select blocked_id::text from public.blocks`); s != alice {
		t.Fatalf("blocked list has %s", s)
	}
	if c := scalar[int](t, d, alice, `select count(*)::int from public.blocks`); c != 0 {
		t.Fatal("blocked user can see the block")
	}
	// Clients cannot insert blocks directly, nor delete someone else's.
	wantCode(t, d.exec(alice, `insert into public.blocks (blocker_id, blocked_id) values (auth.uid(), $1)`, bob), "42501")
	_ = d.exec(alice, `delete from public.blocks`)
	if !scalar[bool](t, d, bob, `select public.is_blocked_with($1)`, alice) {
		t.Fatal("blocked user removed the block")
	}

	// Unblock: Idle, not Connected.
	if !scalar[bool](t, d, bob, `select public.unblock_user($1)`, alice) {
		t.Fatal("unblock removed nothing")
	}
	if d.state(t, bob, alice) != "idle" || d.state(t, alice, bob) != "idle" {
		t.Fatal("unblock should leave idle")
	}
	if err := d.exec(alice, `select public.send_connection_request($1)`, bob); err != nil {
		t.Fatalf("connect after unblock: %v", err)
	}
	// Plain delete of one's own block row also unblocks.
	carol := d.NewUser(t, "Carol C")
	_ = scalar[string](t, d, bob, `select id::text from public.block_user($1)`, carol)
	if err := d.exec(bob, `delete from public.blocks where blocked_id = $1`, carol); err != nil {
		t.Fatal(err)
	}
	if scalar[bool](t, d, bob, `select public.is_blocked_with($1)`, carol) {
		t.Fatal("delete did not unblock")
	}
}

func TestFollow(t *testing.T) {
	d := New(t)
	alice := d.NewUser(t, "Alice A")
	bob := d.NewUser(t, "Bob B")
	count := func() int {
		return scalar[int](t, d, alice, `select followers from public.follow_counts($1)`, bob)
	}
	if count() != 0 {
		t.Fatal("initial followers")
	}
	id := scalar[string](t, d, alice, `select id::text from public.follow_user($1)`, bob)
	if count() != 1 {
		t.Fatalf("followers = %d, want 1", count())
	}
	if again := scalar[string](t, d, alice, `select id::text from public.follow_user($1)`, bob); again != id || count() != 1 {
		t.Fatal("follow is not idempotent")
	}
	if f := scalar[int](t, d, alice, `select following from public.follow_counts(auth.uid())`); f != 1 {
		t.Fatalf("following = %d", f)
	}
	wantCode(t, d.exec(alice, `select public.follow_user(auth.uid())`), "PT400")
	wantCode(t, d.exec(alice, `select public.follow_user(gen_random_uuid())`), "PT404")
	wantCode(t, d.exec(alice, `insert into public.follows (follower_id, followee_id) values (auth.uid(), $1)`, bob), "42501")

	// Connections-only profile: Follow unavailable.
	carol := d.NewUser(t, "Carol C")
	if _, err := d.Pool.Exec(ctx, `update public.profiles set profile_visibility = 'connections' where id = $1`, carol); err != nil {
		t.Fatal(err)
	}
	wantCode(t, d.exec(alice, `select public.follow_user($1)`, carol), "PT403")

	// Only the follower can unfollow.
	_ = d.exec(bob, `delete from public.follows`)
	if count() != 1 {
		t.Fatal("followee removed the follow")
	}
	if err := d.exec(alice, `delete from public.follows where followee_id = $1`, bob); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatal("unfollow did not decrement")
	}
}

func TestMentorshipRequests(t *testing.T) {
	d := New(t)
	alice := d.NewUser(t, "Alice A")
	bob := d.NewUser(t, "Bob B")

	wantCode(t, d.exec(alice, `select public.request_mentorship($1, null, 'hello')`, bob), "PT400")
	wantCode(t, d.exec(alice, `select public.request_mentorship($1, '  ', 'hello')`, bob), "PT400")
	wantCode(t, d.exec(alice, `select public.request_mentorship($1, 'astrology', 'hello')`, bob), "PT400")
	wantCode(t, d.exec(alice, `select public.request_mentorship(auth.uid(), 'career', 'hello')`), "PT400")
	wantCode(t, d.exec(alice, `select public.request_mentorship($1, 'career', repeat('x', 1001))`, bob), "PT400")
	wantCode(t, d.exec(alice, `insert into public.mentorship_requests (requester_id, recipient_id, topic) values (auth.uid(), $1, 'career')`, bob), "42501")

	id := scalar[string](t, d, alice, `select id::text from public.request_mentorship($1, 'system_design', 'Could you review my design?')`, bob)
	if s := scalar[string](t, d, bob, `select status from public.mentorship_requests where id = $1`, id); s != "open" {
		t.Fatalf("status %q", s)
	}
	// Bob is notified.
	got := scalar[string](t, d, bob, `select payload->>'mentorship_request_id' from public.notifications where kind = 'mentorship'`)
	if got != id {
		t.Fatalf("notification payload %q", got)
	}
	// The recipient sets status; the requester cannot.
	if err := d.exec(bob, `update public.mentorship_requests set status = 'accepted' where id = $1`, id); err != nil {
		t.Fatal(err)
	}
	_ = d.exec(alice, `update public.mentorship_requests set status = 'declined' where id = $1`, id)
	if s := scalar[string](t, d, alice, `select status from public.mentorship_requests where id = $1`, id); s != "accepted" {
		t.Fatalf("status %q after requester update", s)
	}
	wantCode(t, d.exec(bob, `update public.mentorship_requests set topic = 'career' where id = $1`, id), "42501")
	carol := d.NewUser(t, "Carol C")
	if n := scalar[int](t, d, carol, `select count(*)::int from public.mentorship_requests`); n != 0 {
		t.Fatal("third party sees mentorship requests")
	}
}

func TestSocialGraphHiddenFromVisitors(t *testing.T) {
	d := New(t)
	alice := d.NewUser(t, "Alice A")
	bob := d.NewUser(t, "Bob B")
	_ = scalar[string](t, d, alice, `select id::text from public.send_connection_request($1)`, bob)
	_ = scalar[string](t, d, alice, `select id::text from public.follow_user($1)`, bob)
	for _, tbl := range []string{"blocks", "connections", "follows", "mentorship_requests"} {
		if n := scalar[int](t, d, "", `select count(*)::int from public.`+tbl); n != 0 {
			t.Fatalf("anon reads %d rows of %s", n, tbl)
		}
	}
	wantCode(t, d.exec("", `select public.block_user($1)`, bob), "42501")
}
