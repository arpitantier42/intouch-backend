package dbtest

import (
	"testing"

	"github.com/intouch/api/internal/authattempt"
	"github.com/jackc/pgx/v5"
)

// service runs one query as service_role (the API service) and scans one value.
func service[T any](d *DB, sql string, args ...any) (T, error) {
	var v T
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return v, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local role service_role"); err != nil {
		return v, err
	}
	if err := tx.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		return v, err
	}
	return v, tx.Commit(ctx)
}

func mustService[T any](t *testing.T, d *DB, sql string, args ...any) T {
	t.Helper()
	v, err := service[T](d, sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

const succeed = `select 'ok' from public.auth_attempt_succeeded($1::uuid)`

// backdate inserts an attempt row created ago (an interval) in the past.
func (d *DB) backdate(t *testing.T, hash, action string, otpRef any, succeeded bool, ago string) {
	t.Helper()
	if _, err := d.Pool.Exec(ctx, `insert into public.auth_attempts (identifier_hash, action, otp_ref, succeeded, created_at)
		values ($1, $2, $3, $4, now() - $5::interval)`, hash, action, otpRef, succeeded, ago); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

func TestOTPThrottles(t *testing.T) {
	d := New(t)
	h, _ := authattempt.IdentifierHash("asha@example.com")

	// First send is accepted; a resend 20 s later is throttled.
	mustService[string](t, d, `select public.auth_otp_send($1)::text`, h)
	_, err := service[string](d, `select public.auth_otp_send($1)::text`, h)
	wantCode(t, err, "PT429")

	other, _ := authattempt.IdentifierHash("9876543210")
	d.backdate(t, other, "otp_send", "6f1c1d1e-0000-4000-8000-000000000001", true, "20 seconds")
	_, err = service[string](d, `select public.auth_otp_send($1)::text`, other)
	wantCode(t, err, "PT429")
	d.backdate(t, other, "otp_send", "6f1c1d1e-0000-4000-8000-000000000002", true, "31 seconds")
	if _, err := d.Pool.Exec(ctx, `delete from public.auth_attempts where otp_ref = '6f1c1d1e-0000-4000-8000-000000000001'`); err != nil {
		t.Fatal(err)
	}
	mustService[string](t, d, `select public.auth_otp_send($1)::text`, other)

	// Five verifies on one OTP are allowed; the 6th is rejected.
	for i := 0; i < 5; i++ {
		mustService[string](t, d, `select public.auth_otp_verify_begin($1)::text`, h)
	}
	_, err = service[string](d, `select public.auth_otp_verify_begin($1)::text`, h)
	wantCode(t, err, "PT429")

	// An 11-minute-old OTP is expired.
	old, _ := authattempt.IdentifierHash("old@example.com")
	d.backdate(t, old, "otp_send", "6f1c1d1e-0000-4000-8000-000000000003", true, "11 minutes")
	_, err = service[string](d, `select public.auth_otp_verify_begin($1)::text`, old)
	wantCode(t, err, "PT410")
	// A 9-minute-old one is still valid, and a verify can be marked successful.
	fresh, _ := authattempt.IdentifierHash("fresh@example.com")
	d.backdate(t, fresh, "otp_send", "6f1c1d1e-0000-4000-8000-000000000004", true, "9 minutes")
	id := mustService[string](t, d, `select public.auth_otp_verify_begin($1)::text`, fresh)
	mustService[string](t, d, succeed, id)
	var ok bool
	_ = d.Pool.QueryRow(ctx, `select succeeded from public.auth_attempts where id = $1`, id).Scan(&ok)
	if !ok {
		t.Fatal("verify attempt not marked succeeded")
	}

	// No OTP sent to this identifier: wrong code.
	none, _ := authattempt.IdentifierHash("none@example.com")
	_, err = service[string](d, `select public.auth_otp_verify_begin($1)::text`, none)
	wantCode(t, err, "PT401")

	// Clients can neither read the log nor call the throttle functions.
	u := d.NewUser(t, "Asha Rao")
	wantCode(t, d.exec(u, `select count(*) from public.auth_attempts`), "42501")
	wantCode(t, d.exec("", `select count(*) from public.auth_attempts`), "42501")
	wantCode(t, d.exec(u, `select public.auth_otp_send($1)`, h), "42501")
	wantCode(t, d.exec(u, `select public.auth_password_begin($1)`, h), "42501")
}

func TestPasswordLockout(t *testing.T) {
	d := New(t)
	h, _ := authattempt.IdentifierHash("lock@example.com")

	// Nine failures: still allowed; the 10th failure locks.
	for i := 0; i < 9; i++ {
		d.backdate(t, h, "password", nil, false, "5 minutes")
	}
	if n := mustService[int](t, d, `select public.auth_password_failures($1)`, h); n != 9 {
		t.Fatalf("failures = %d, want 9", n)
	}
	mustService[string](t, d, `select public.auth_password_begin($1)::text`, h) // 10th, fails
	_, err := service[string](d, `select public.auth_password_begin($1)::text`, h)
	wantCode(t, err, "PT423")
	// Blocked attempts are not logged.
	if n := mustService[int](t, d, `select public.auth_password_failures($1)`, h); n != 10 {
		t.Fatalf("failures = %d, want 10", n)
	}

	// Ten failures that ended 16 minutes ago: the 15-minute block is over.
	h2, _ := authattempt.IdentifierHash("expired@example.com")
	for i := 0; i < 10; i++ {
		d.backdate(t, h2, "password", nil, false, "16 minutes")
	}
	id := mustService[string](t, d, `select public.auth_password_begin($1)::text`, h2)
	// A success resets the counter to zero.
	mustService[string](t, d, succeed, id)
	if n := mustService[int](t, d, `select public.auth_password_failures($1)`, h2); n != 0 {
		t.Fatalf("failures after success = %d, want 0", n)
	}

	// Ten failures spread over more than 15 minutes do not lock.
	h3, _ := authattempt.IdentifierHash("spread@example.com")
	for i := 0; i < 5; i++ {
		d.backdate(t, h3, "password", nil, false, "20 minutes")
	}
	for i := 0; i < 5; i++ {
		d.backdate(t, h3, "password", nil, false, "1 minute")
	}
	mustService[string](t, d, `select public.auth_password_begin($1)::text`, h3)

	// Failures before a success do not count towards the lock.
	h4, _ := authattempt.IdentifierHash("reset@example.com")
	for i := 0; i < 9; i++ {
		d.backdate(t, h4, "password", nil, false, "3 minutes")
	}
	d.backdate(t, h4, "password", nil, true, "2 minutes")
	d.backdate(t, h4, "password", nil, false, "1 minute")
	mustService[string](t, d, `select public.auth_password_begin($1)::text`, h4)
	if n := mustService[int](t, d, `select public.auth_password_failures($1)`, h4); n != 2 {
		t.Fatalf("failures after reset = %d, want 2", n)
	}
}

func TestKnownDevicesAndLoginAlerts(t *testing.T) {
	d := New(t)
	u := d.NewUser(t, "Asha Rao")
	other := d.NewUser(t, "Ravi Iyer")
	phone := authattempt.Hash("phone-fingerprint")
	laptop := authattempt.Hash("laptop-fingerprint")
	const s1, s2, s3 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"

	type result struct {
		New     bool `json:"new_device"`
		Alerted bool `json:"alerted"`
	}
	signIn := func(user, session, device string) result {
		t.Helper()
		return mustService[result](t, d, `select public.record_sign_in($1, $2, $3, 'Test agent')`, user, session, device)
	}

	// First sign-in from the phone is new; with alerts on (default) it alerts.
	if r := signIn(u, s1, phone); !r.New || !r.Alerted {
		t.Fatalf("first sign-in: %+v", r)
	}
	// Same device, new session: not new, no alert.
	if r := signIn(u, s2, phone); r.New || r.Alerted {
		t.Fatalf("known device: %+v", r)
	}
	// A new device with alerts off: new, no alert.
	if _, err := d.Pool.Exec(ctx, `update public.profiles set login_alerts = false where id = $1`, u); err != nil {
		t.Fatal(err)
	}
	if r := signIn(u, s3, laptop); !r.New || r.Alerted {
		t.Fatalf("alerts off: %+v", r)
	}
	if n := scalar[int](t, d, u, `select count(*)::int from public.notifications where kind = 'login_alert'`); n != 1 {
		t.Fatalf("login alerts = %d, want 1", n)
	}
	// A session id cannot be claimed by another user.
	_, err := service[result](d, `select public.record_sign_in($1, $2, $3, 'x')`, other, s1, phone)
	wantCode(t, err, "PT409")

	// The owner sees their devices without device_hash; others see none.
	if n := scalar[int](t, d, u, `select count(*)::int from (select id, session_id, user_agent, last_seen_at from public.known_devices) x`); n != 3 {
		t.Fatalf("own devices = %d, want 3", n)
	}
	wantCode(t, d.exec(u, `select device_hash from public.known_devices`), "42501")
	if n := scalar[int](t, d, other, `select count(id)::int from public.known_devices`); n != 0 {
		t.Fatalf("other user sees %d devices", n)
	}
	if n := scalar[int](t, d, "", `select count(id)::int from public.known_devices`); n != 0 {
		t.Fatalf("anon sees %d devices", n)
	}
	wantCode(t, d.exec(u, `delete from public.known_devices`), "42501")
	wantCode(t, d.exec(u, `insert into public.known_devices (user_id, session_id, device_hash) values ($1, gen_random_uuid(), $2)`, u, phone), "42501")

	// Sign out of all other devices: only the current session remains.
	var removed []string
	tx, _ := d.Pool.Begin(ctx)
	rows, err := tx.Query(ctx, `select s::text from public.forget_other_devices($1, $2) s`, u, s3)
	if err != nil {
		t.Fatal(err)
	}
	removed, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	if len(removed) != 2 {
		t.Fatalf("removed %v, want 2 sessions", removed)
	}
	if got := scalar[string](t, d, u, `select string_agg(session_id::text, ',') from public.known_devices`); got != s3 {
		t.Fatalf("remaining sessions %q, want %q", got, s3)
	}
}

func TestNotificationsInbox(t *testing.T) {
	d := New(t)
	u := d.NewUser(t, "Asha Rao")
	other := d.NewUser(t, "Ravi Iyer")
	id := mustService[string](t, d, `select public.notify($1, 'job_interest', '{"job_id": "x"}')::text`, u)

	if n := scalar[int](t, d, u, `select count(*)::int from public.notifications`); n != 1 {
		t.Fatalf("own notifications = %d", n)
	}
	if n := scalar[int](t, d, other, `select count(*)::int from public.notifications`); n != 0 {
		t.Fatalf("other sees %d", n)
	}
	if n := scalar[int](t, d, "", `select count(*)::int from public.notifications`); n != 0 {
		t.Fatalf("anon sees %d", n)
	}
	// Mark read is the only client write.
	if err := d.exec(u, `update public.notifications set read_at = now() where id = $1`, id); err != nil {
		t.Fatal(err)
	}
	wantCode(t, d.exec(u, `update public.notifications set kind = 'x' where id = $1`, id), "42501")
	wantCode(t, d.exec(u, `insert into public.notifications (user_id, kind) values ($1, 'fake')`, u), "42501")
	wantCode(t, d.exec(u, `select public.notify($1, 'fake', '{}')`, other), "42501")
	if err := d.exec(other, `update public.notifications set read_at = null where id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if !scalar[bool](t, d, u, `select read_at is not null from public.notifications where id = $1`, id) {
		t.Fatal("another user changed my notification")
	}
	// Kind and payload shape are checked.
	_, err := service[string](d, `select public.notify($1, 'Bad Kind', '{}')::text`, u)
	wantCode(t, err, "23514")
	_, err = service[string](d, `select public.notify($1, 'ok', '[1]')::text`, u)
	wantCode(t, err, "23514")
}

func TestSwitcherListsJoinedCommunitiesOnly(t *testing.T) {
	d := New(t)
	owner := d.NewUser(t, "Owner One")
	u := d.NewUser(t, "Asha Rao")
	joined := d.createCommunity(t, owner, "Joined Club", "public")
	pending := d.createCommunity(t, owner, "Pending Club", "public")
	d.addMember(t, joined, u, "moderator")
	if err := d.exec(u, `select public.request_join($1)`, pending); err != nil {
		t.Fatal(err)
	}
	got := scalar[string](t, d, u, `select string_agg(name || ':' || role, ',') from public.switcher_communities()`)
	if got != "Joined Club:moderator" {
		t.Fatalf("switcher = %q", got)
	}
	// Deleted communities drop out too.
	if _, err := d.Pool.Exec(ctx, `update public.communities set deleted_at = now() where id = $1`, joined); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, d, u, `select count(*)::int from public.switcher_communities()`); n != 0 {
		t.Fatalf("switcher after delete = %d", n)
	}
	if n := scalar[int](t, d, owner, `select count(*)::int from public.switcher_communities()`); n != 1 {
		t.Fatalf("owner switcher = %d, want 1", n)
	}
	wantCode(t, d.exec("", `select * from public.switcher_communities()`), "42501")
}
