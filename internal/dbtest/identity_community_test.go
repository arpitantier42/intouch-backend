package dbtest

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

var ctx = context.Background()

// exec runs one statement as uid and returns its error.
func (d *DB) exec(uid, sql string, args ...any) error {
	return d.As(uid, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// scalar runs a single-value query as uid.
func scalar[T any](t *testing.T, d *DB, uid, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := d.As(uid, func(tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(&v) }); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := SQLState(err); got != code {
		t.Fatalf("want SQLSTATE %s, got %v", code, err)
	}
}

// createCommunity creates a community through the RPC as owner.
func (d *DB) createCommunity(t *testing.T, owner, name, visibility string) string {
	t.Helper()
	return scalar[string](t, d, owner,
		`select id::text from public.create_community($1, 'online', gen_random_uuid(), null, $2)`, name, visibility)
}

// addMember inserts a joined membership directly (as superuser).
func (d *DB) addMember(t *testing.T, community, user, role string) {
	t.Helper()
	if _, err := d.Pool.Exec(ctx, `insert into public.memberships (community_id, user_id, role, status, via)
		values ($1, $2, $3, 'joined', 'direct')`, community, user, role); err != nil {
		t.Fatalf("add member: %v", err)
	}
}

func (d *DB) invite(t *testing.T, community, by, extra string) string {
	t.Helper()
	var code string
	err := d.Pool.QueryRow(ctx, `insert into public.invitations (community_id, channel, created_by`+extra+`)
		values ($1, 'link', $2`+strings.Repeat(", default", strings.Count(extra, ","))+`) returning code`,
		community, by).Scan(&code)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	return code
}

func TestRLSEnabledOnEveryPublicTable(t *testing.T) {
	d := New(t)
	var missing []string
	rows, err := d.Pool.Query(ctx, `select c.relname from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = 'public' and c.relkind = 'r' and not c.relrowsecurity`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		missing = append(missing, s)
	}
	if len(missing) > 0 {
		t.Fatalf("tables without RLS: %v", missing)
	}
}

func TestMembershipScopingAndRoles(t *testing.T) {
	d := New(t)
	alice, bob, carol := d.NewUser(t, "Alice A"), d.NewUser(t, "Bob B"), d.NewUser(t, "Carol C")
	a := d.createCommunity(t, alice, "Alpha", "private")
	b := d.createCommunity(t, bob, "Beta", "private")
	d.addMember(t, b, alice, "member")

	// AC1: a member of two communities sees both in one session.
	if n := scalar[int](t, d, alice, `select count(*)::int from public.communities`); n != 2 {
		t.Fatalf("alice sees %d communities, want 2", n)
	}
	if !scalar[bool](t, d, alice, `select public.is_joined($1) and public.is_joined($2)`, a, b) {
		t.Fatal("alice should be joined to both")
	}

	// AC2: a non-member sees no rows of a private community.
	for _, q := range []string{
		`select count(*)::int from public.communities where id = $1`,
		`select count(*)::int from public.memberships where community_id = $1`,
		`select count(*)::int from public.invitations where community_id = $1`,
		`select count(*)::int from public.community_questions where community_id = $1`,
	} {
		if n := scalar[int](t, d, carol, q, a); n != 0 {
			t.Fatalf("non-member: %s returned %d rows", q, n)
		}
	}
	if scalar[bool](t, d, carol, `select public.is_joined($1)`, a) {
		t.Fatal("carol is not joined")
	}
	if n := scalar[int](t, d, "", `select count(*)::int from public.communities`); n != 0 {
		t.Fatalf("anon sees %d communities", n)
	}

	// AC3: owner of A is only a member in B, so admin actions in B fail.
	if _, err := d.Pool.Exec(ctx, `insert into public.memberships (community_id, user_id, status, via) values ($1, $2, 'pending', 'request')`, b, carol); err != nil {
		t.Fatal(err)
	}
	wantCode(t, d.exec(alice, `select public.review_join_request($1, $2, 'approve')`, b, carol), "PT403")
	wantCode(t, d.exec(alice, `select public.transfer_ownership($1, $2)`, b, alice), "PT403")

	// AC4, AC5: role and status outside the allowed sets.
	_, err := d.Pool.Exec(ctx, `update public.memberships set role = 'superadmin' where community_id = $1 and user_id = $2`, b, alice)
	wantCode(t, err, "23514")
	_, err = d.Pool.Exec(ctx, `update public.memberships set status = 'banned_x' where community_id = $1 and user_id = $2`, b, alice)
	wantCode(t, err, "23514")

	// AC6: a second owner is rejected by the partial unique index, and API
	// users cannot write role at all.
	_, err = d.Pool.Exec(ctx, `update public.memberships set role = 'owner' where community_id = $1 and user_id = $2`, b, alice)
	wantCode(t, err, "23505")
	wantCode(t, d.exec(alice, `update public.memberships set role = 'owner' where community_id = $1 and user_id = $2`, b, alice), "42501")

	// AC19: inserting one's own joined (or any) membership via the API fails.
	wantCode(t, d.exec(carol, `insert into public.memberships (community_id, user_id, role, status, via) values ($1, $2, 'member', 'joined', 'direct')`, a, carol), "42501")

	// Owners cannot leave; members can, and member_count follows.
	if n := scalar[int](t, d, bob, `with x as (delete from public.memberships where community_id = $1 and user_id = $2 returning 1) select count(*)::int from x`, b, bob); n != 0 {
		t.Fatal("owner must not be able to leave")
	}
	var count int
	_ = d.Pool.QueryRow(ctx, `select member_count from public.communities where id = $1`, b).Scan(&count)
	if count != 2 {
		t.Fatalf("member_count = %d, want 2 (pending excluded)", count)
	}
	if err := d.exec(alice, `delete from public.memberships where community_id = $1 and user_id = auth.uid()`, b); err != nil {
		t.Fatal(err)
	}
	_ = d.Pool.QueryRow(ctx, `select member_count from public.communities where id = $1`, b).Scan(&count)
	if count != 1 {
		t.Fatalf("member_count after leave = %d, want 1", count)
	}
}

func TestCreateAndTransfer(t *testing.T) {
	d := New(t)
	alice, bob := d.NewUser(t, "Alice A"), d.NewUser(t, "Bob B")
	key := "7f9c2ba4-e88f-4d5a-9e0b-3f6a1c2d4e5f"
	id := scalar[string](t, d, alice, `select id::text from public.create_community('Pune Runners Club', 'place', $1, 'Pune')`, key)
	// Idempotent on creation_key.
	if again := scalar[string](t, d, alice, `select id::text from public.create_community('Pune Runners Club', 'place', $1, 'Pune')`, key); again != id {
		t.Fatal("double create made two communities")
	}

	// AC8 and REQ-020: creator is owner, joined, via direct; defaults applied.
	var role, status, via, handle, visibility string
	var members int
	_ = d.Pool.QueryRow(ctx, `select m.role, m.status, m.via, c.handle, c.visibility, c.member_count
		from public.memberships m join public.communities c on c.id = m.community_id
		where m.community_id = $1 and m.user_id = $2`, id, alice).Scan(&role, &status, &via, &handle, &visibility, &members)
	if role != "owner" || status != "joined" || via != "direct" {
		t.Fatalf("creator membership = %s/%s/%s", role, status, via)
	}
	if handle != "pune-runners-club" || visibility != "private" || members != 1 {
		t.Fatalf("community = %s/%s/%d", handle, visibility, members)
	}
	h2 := scalar[string](t, d, bob, `select handle from public.create_community('Pune Runners Club!', 'online', gen_random_uuid())`)
	if h2 != "pune-runners-club-2" {
		t.Fatalf("collision handle = %s", h2)
	}
	wantCode(t, d.exec(alice, `select public.create_community('  ', 'online', gen_random_uuid())`), "PT400")

	// AC9: a community whose owner membership is never written does not survive.
	var err error
	tx, _ := d.Pool.Begin(ctx)
	_, _ = tx.Exec(ctx, `insert into public.communities (handle, name, location_type, created_by, creation_key)
		values ('orphan', 'Orphan', 'online', $1, gen_random_uuid())`, alice)
	wantCode(t, tx.Commit(ctx), "23514")
	ghost := "00000000-0000-0000-0000-00000000dead"
	if _, err := d.Pool.Exec(ctx, `insert into auth.users (id) values ($1)`, ghost); err != nil {
		t.Fatal(err)
	}
	// Caller without a profile: the membership insert fails, so the whole RPC rolls back.
	if err := d.exec(ghost, `select public.create_community('Ghost town', 'online', gen_random_uuid())`); err == nil {
		t.Fatal("create without profile should fail")
	}
	var n int
	_ = d.Pool.QueryRow(ctx, `select count(*) from public.communities where name in ('Orphan', 'Ghost town')`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d orphan communities remain", n)
	}

	// The owner cannot be removed or demoted outside a transfer.
	_, err = d.Pool.Exec(ctx, `update public.memberships set role = 'admin' where community_id = $1 and user_id = $2`, id, alice)
	wantCode(t, err, "23514")

	// AC7: transfer makes bob owner and alice admin.
	wantCode(t, d.exec(alice, `select public.transfer_ownership($1, $2)`, id, bob), "PT422")
	d.addMember(t, id, bob, "member")
	if err := d.exec(alice, `select public.transfer_ownership($1, $2)`, id, bob); err != nil {
		t.Fatal(err)
	}
	var aliceRole, bobRole string
	_ = d.Pool.QueryRow(ctx, `select
		(select role from public.memberships where community_id = $1 and user_id = $2),
		(select role from public.memberships where community_id = $1 and user_id = $3)`, id, alice, bob).Scan(&aliceRole, &bobRole)
	if aliceRole != "admin" || bobRole != "owner" {
		t.Fatalf("after transfer alice=%s bob=%s", aliceRole, bobRole)
	}
}

func TestProfileRules(t *testing.T) {
	d := New(t)
	u1, u2 := d.NewUser(t, "Elena Larsen"), d.NewUser(t, "Diego Silva")

	// AC14: new profiles start with onboarding incomplete.
	if scalar[bool](t, d, u1, `select onboarding_complete from public.profiles where id = auth.uid()`) {
		t.Fatal("onboarding_complete should default to false")
	}
	set := func(uid, col string, v any) error {
		return d.exec(uid, `update public.profiles set `+col+` = $1 where id = auth.uid()`, v)
	}
	wantCode(t, set(u1, "username", "elena l"), "23514")           // AC10: space
	wantCode(t, set(u1, "username", "el"), "23514")                // AC11: 2 chars
	wantCode(t, set(u1, "bio", strings.Repeat("x", 200)), "22001") // AC13: bio 200
	wantCode(t, set(u1, "username", "elena.l"), "")
	wantCode(t, set(u2, "username", "elena.l"), "23505") // AC16
	wantCode(t, set(u1, "is_operator", true), "42501")   // server-set
	wantCode(t, set(u1, "deletion_requested_at", "2026-01-01"), "42501")
	if _, err := d.Pool.Exec(ctx, `update public.profiles set username = 'diego' where id = $1`, u2); err != nil {
		t.Fatal(err)
	}
	// Another user's row is not updatable.
	if n := scalar[int](t, d, u1, `with x as (update public.profiles set full_name = 'Hacked' where id = $1 returning 1) select count(*)::int from x`, u2); n != 0 {
		t.Fatal("updated someone else's profile")
	}

	// AC15: two concurrent saves of the same username, exactly one succeeds.
	u3, u4 := d.NewUser(t, "Ana P"), d.NewUser(t, "Ben Q")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, u := range []string{u3, u4} {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			errs[i] = set(u, "username", "same.name")
		}(i, u)
	}
	wg.Wait()
	ok := 0
	for _, e := range errs {
		if e == nil {
			ok++
		} else {
			wantCode(t, e, "23505")
		}
	}
	if ok != 1 {
		t.Fatalf("%d concurrent saves succeeded, want 1", ok)
	}
}

func TestQuestionsAndJoinRequests(t *testing.T) {
	d := New(t)
	owner, admin, applicant, member := d.NewUser(t, "Olga O"), d.NewUser(t, "Adam A"), d.NewUser(t, "Priya P"), d.NewUser(t, "Mo M")
	c := d.createCommunity(t, owner, "Open Club", "public")
	d.addMember(t, c, admin, "admin")
	d.addMember(t, c, member, "member")

	q := func(kind, options string) error {
		_, err := d.Pool.Exec(ctx, `insert into public.community_questions (community_id, label, kind, options, required)
			values ($1, 'Q', $2, $3::jsonb, true)`, c, kind, options)
		return err
	}
	wantCode(t, q("file", "null"), "23514")          // AC17
	wantCode(t, q("single_choice", "null"), "23514") // AC18
	wantCode(t, q("multi_choice", "[]"), "23514")
	var required, optional string
	_ = d.Pool.QueryRow(ctx, `insert into public.community_questions (community_id, label, kind, required, position)
		values ($1, 'Where do you work?', 'short_text', true, 0) returning id::text`, c).Scan(&required)
	_ = d.Pool.QueryRow(ctx, `insert into public.community_questions (community_id, label, kind, required, position, options)
		values ($1, 'Pick one', 'single_choice', false, 1, '["a","b"]') returning id::text`, c).Scan(&optional)

	// AC21: a blank required answer blocks the request, and nothing is stored.
	blank := `[{"question_id":"` + required + `","value":"  "}]`
	wantCode(t, d.exec(applicant, `select public.request_join($1, $2::jsonb)`, c, blank), "PT400")
	if n := scalar[int](t, d, applicant, `select count(*)::int from public.memberships where user_id = auth.uid()`); n != 0 {
		t.Fatal("blank submission left a membership")
	}
	good := `[{"question_id":"` + required + `","value":"Acme"}]`
	if err := d.exec(applicant, `select public.request_join($1, $2::jsonb)`, c, good); err != nil {
		t.Fatal(err)
	}
	wantCode(t, d.exec(applicant, `select public.request_join($1, $2::jsonb)`, c, good), "PT409")

	// AC22: owner/admin see the answers; ordinary members do not.
	for uid, want := range map[string]int{admin: 1, owner: 1, applicant: 1, member: 0} {
		if n := scalar[int](t, d, uid, `select count(*)::int from public.join_answers`); n != want {
			t.Fatalf("join_answers visible = %d, want %d", n, want)
		}
	}
	if n := scalar[int](t, d, member, `select count(*)::int from public.memberships where status = 'pending'`); n != 0 {
		t.Fatal("members must not see pending requests")
	}

	// No self review; members cannot review.
	wantCode(t, d.exec(member, `select public.review_join_request($1, $2, 'approve')`, c, applicant), "PT403")
	wantCode(t, d.exec(applicant, `select public.review_join_request($1, $2, 'approve')`, c, applicant), "PT403")

	// AC20: approval makes the membership joined via request.
	if err := d.exec(admin, `select public.review_join_request($1, $2, 'approve')`, c, applicant); err != nil {
		t.Fatal(err)
	}
	var status, via, reviewer string
	_ = d.Pool.QueryRow(ctx, `select status, via, reviewed_by::text from public.memberships where community_id = $1 and user_id = $2`,
		c, applicant).Scan(&status, &via, &reviewer)
	if status != "joined" || via != "request" || reviewer != admin {
		t.Fatalf("approved membership = %s/%s/%s", status, via, reviewer)
	}

	// Private communities are invite only.
	p := d.createCommunity(t, owner, "Secret", "private")
	wantCode(t, d.exec(member, `select public.request_join($1)`, p), "PT403")
}

func TestInvitations(t *testing.T) {
	d := New(t)
	owner, guest, other := d.NewUser(t, "Olga O"), d.NewUser(t, "Gita G"), d.NewUser(t, "Omar O")
	c := d.createCommunity(t, owner, "Invite Only", "private")
	c2 := d.createCommunity(t, owner, "Elsewhere", "private")

	// AC23: 10,000 generated codes are all distinct (and 32 hex chars).
	var total, distinct, badLen int
	err := d.Pool.QueryRow(ctx, `with ins as (
			insert into public.invitations (community_id, channel, created_by)
			select $1, 'link', $2 from generate_series(1, 10000) returning code)
		select count(*), count(distinct code), count(*) filter (where code !~ '^[0-9a-f]{32}$') from ins`,
		c, owner).Scan(&total, &distinct, &badLen)
	if err != nil || total != 10000 || distinct != 10000 || badLen != 0 {
		t.Fatalf("codes: total=%d distinct=%d bad=%d err=%v", total, distinct, badLen, err)
	}
	if n := scalar[int](t, d, guest, `select count(*)::int from public.invitations`); n != 0 {
		t.Fatal("non-members must not list invitations")
	}

	code := d.invite(t, c, owner, "")
	// AC24: the code only works with its own community id.
	wantCode(t, d.exec("", `select public.open_invitation($1, $2)`, c2, code), "PT404")
	wantCode(t, d.exec(guest, `select public.accept_invitation($1, $2)`, c2, code), "PT404")

	// AC28: opening (anonymously) marks it opened.
	if err := d.exec("", `select public.open_invitation($1, $2)`, c, code); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = d.Pool.QueryRow(ctx, `select tracking_state from public.invitations where code = $1`, code).Scan(&state)
	if state != "opened" {
		t.Fatalf("tracking_state = %s, want opened", state)
	}

	// Accept: joined via invite, use_count bumped; AC26: the next use is rejected.
	if err := d.exec(guest, `select public.accept_invitation($1, $2)`, c, code); err != nil {
		t.Fatal(err)
	}
	var via, invID string
	var uses int
	_ = d.Pool.QueryRow(ctx, `select m.via, coalesce(m.invitation_id::text, ''), i.use_count, i.tracking_state
		from public.memberships m join public.invitations i on i.id = m.invitation_id
		where m.community_id = $1 and m.user_id = $2`, c, guest).Scan(&via, &invID, &uses, &state)
	if via != "invite" || invID == "" || uses != 1 || state != "accepted" {
		t.Fatalf("accepted: via=%s uses=%d state=%s", via, uses, state)
	}
	wantCode(t, d.exec(other, `select public.accept_invitation($1, $2)`, c, code), "PT410")
	if !scalar[bool](t, d, guest, `select public.is_joined($1)`, c) {
		t.Fatal("guest should be joined")
	}

	// AC25: expired. AC27: revoked. Both rejected; the landing page says 404.
	expired := d.invite(t, c, owner, "")
	_, _ = d.Pool.Exec(ctx, `update public.invitations set expires_at = now() - interval '1 minute' where code = $1`, expired)
	wantCode(t, d.exec(other, `select public.accept_invitation($1, $2)`, c, expired), "PT410")
	wantCode(t, d.exec("", `select public.open_invitation($1, $2)`, c, expired), "PT404")
	revoked := d.invite(t, c, owner, "")
	_, _ = d.Pool.Exec(ctx, `update public.invitations set revoked_at = now() where code = $1`, revoked)
	wantCode(t, d.exec(other, `select public.accept_invitation($1, $2)`, c, revoked), "PT410")
	wantCode(t, d.exec("", `select public.open_invitation($1, $2)`, c, revoked), "PT404")

	// Required questions apply to invite acceptance too (REQ-190).
	var qid string
	_ = d.Pool.QueryRow(ctx, `insert into public.community_questions (community_id, label, kind, required)
		values ($1, 'Why join?', 'long_text', true) returning id::text`, c).Scan(&qid)
	fresh := d.invite(t, c, owner, "")
	wantCode(t, d.exec(other, `select public.accept_invitation($1, $2)`, c, fresh), "PT400")
	if err := d.exec(other, `select public.accept_invitation($1, $2, $3::jsonb)`, c, fresh,
		`[{"question_id":"`+qid+`","value":"To meet people"}]`); err != nil {
		t.Fatal(err)
	}
}
