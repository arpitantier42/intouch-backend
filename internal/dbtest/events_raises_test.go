package dbtest

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// newEvent seeds a free online event (as service_role, standing in for the
// create-event RPC) starting in `in` and returns its id.
func (d *DB) newEvent(t *testing.T, community, creator string, seatCap any, in string) string {
	t.Helper()
	return mustService[string](t, d, `insert into public.events
		(community_id, creator_id, title, starts_at, ends_at, format, join_url, seat_cap)
		values ($1, $2, 'Meetup', now() + $4::interval, now() + $4::interval + interval '2 hours',
		        'online', 'https://meet.example.com/x', $3)
		returning id::text`, community, creator, seatCap, in)
}

// rsvp upserts an RSVP as the RSVP RPC would (service_role).
func (d *DB) rsvp(event, user, status string, push, email bool) error {
	_, err := service[string](d, `insert into public.rsvps (event_id, user_id, status, remind_push, remind_email)
		values ($1, $2, $3, $4, $5)
		on conflict (event_id, user_id) do update
		  set status = excluded.status, remind_push = excluded.remind_push, remind_email = excluded.remind_email
		returning id::text`, event, user, status, push, email)
	return err
}

func eventsFixture(t *testing.T) (d *DB, owner, member, outsider, c string) {
	d = New(t)
	owner = d.NewUser(t, "Olga Owner")
	member = d.NewUser(t, "Max Member")
	outsider = d.NewUser(t, "Otto Outsider")
	c = d.createCommunity(t, owner, "Events", "private")
	d.addMember(t, c, member, "member")
	return
}

func TestEventPublishRules(t *testing.T) {
	d, owner, _, _, c := eventsFixture(t)
	ins := func(cols, vals string) error {
		_, err := service[string](d, `insert into public.events (community_id, creator_id, title, starts_at, ends_at, format, venue`+cols+`)
			values ($1, $2, 'Talk', now() + interval '1 day', now() + interval '1 day 2 hours', 'in_person', 'Hall'`+vals+`)
			returning id::text`, c, owner)
		return err
	}
	// Paid without a payment link: publish blocked.
	wantCode(t, ins(", is_paid, price_minor", ", true, 50000"), "23514")
	// Payment link not http(s): rejected.
	wantCode(t, ins(", is_paid, price_minor, payment_url", ", true, 50000, 'ftp://pay.example.com'"), "23514")
	wantCode(t, ins(", is_paid, price_minor, payment_url", ", true, 50000, 'javascript:alert(1)'"), "23514")
	// Paid with zero price, free with a price, seat cap 0: rejected.
	wantCode(t, ins(", is_paid, price_minor, payment_url", ", true, 0, 'https://pay.example.com'"), "23514")
	wantCode(t, ins(", price_minor", ", 100"), "23514")
	wantCode(t, ins(", seat_cap", ", 0"), "23514")
	// Valid paid event.
	if err := ins(", is_paid, price_minor, payment_url", ", true, 50000, 'https://pay.example.com/e1'"); err != nil {
		t.Fatalf("paid event: %v", err)
	}
	// Blank title, end before start, online without link, in person without venue.
	_, err := service[string](d, `insert into public.events (community_id, creator_id, title, starts_at, ends_at, format, venue)
		values ($1, $2, '  ', now(), now() + interval '1 hour', 'in_person', 'Hall') returning id::text`, c, owner)
	wantCode(t, err, "23514")
	_, err = service[string](d, `insert into public.events (community_id, creator_id, title, starts_at, ends_at, format, venue)
		values ($1, $2, 'T', now(), now() - interval '1 hour', 'in_person', 'Hall') returning id::text`, c, owner)
	wantCode(t, err, "23514")
	_, err = service[string](d, `insert into public.events (community_id, creator_id, title, starts_at, ends_at, format)
		values ($1, $2, 'T', now(), now() + interval '1 hour', 'online') returning id::text`, c, owner)
	wantCode(t, err, "23514")
	_, err = service[string](d, `insert into public.events (community_id, creator_id, title, starts_at, ends_at, format)
		values ($1, $2, 'T', now(), now() + interval '1 hour', 'in_person') returning id::text`, c, owner)
	wantCode(t, err, "23514")
}

func TestEventVisibilityAndEdits(t *testing.T) {
	d, owner, member, outsider, c := eventsFixture(t)
	e := d.newEvent(t, c, owner, 10, "2 days")

	if n := scalar[int](t, d, member, `select count(*)::int from public.events`); n != 1 {
		t.Fatalf("member sees %d events", n)
	}
	for _, u := range []string{outsider, ""} {
		if n := scalar[int](t, d, u, `select count(*)::int from public.events`); n != 0 {
			t.Fatalf("%q sees %d events", u, n)
		}
	}
	// join_url is not readable directly; only via event_join_url.
	wantCode(t, d.exec(member, `select join_url from public.events`), "42501")
	if u := scalar[*string](t, d, member, `select public.event_join_url($1)`, e); u != nil {
		t.Fatalf("member without seat gets join url %v", *u)
	}
	if u := scalar[*string](t, d, owner, `select public.event_join_url($1)`, e); u == nil {
		t.Fatal("creator cannot read join url")
	}
	if err := d.rsvp(e, member, "going", true, true); err != nil {
		t.Fatal(err)
	}
	if u := scalar[*string](t, d, member, `select public.event_join_url($1)`, e); u == nil {
		t.Fatal("confirmed seat cannot read join url")
	}

	// Clients cannot insert events or rsvps, or set server columns.
	wantCode(t, d.exec(member, `insert into public.events (community_id, creator_id, title, starts_at, ends_at, format, venue)
		values ($1, $2, 'X', now(), now() + interval '1 hour', 'in_person', 'v')`, c, member), "42501")
	wantCode(t, d.exec(member, `insert into public.rsvps (event_id, user_id, status) values ($1, $2, 'going')`, e, member), "42501")
	wantCode(t, d.exec(owner, `update public.events set confirmed_count = 0 where id = $1`, e), "42501")
	wantCode(t, d.exec(owner, `update public.events set deleted_at = now() where id = $1`, e), "42501")

	// Creator edits; others' updates touch nothing.
	if err := d.exec(member, `update public.events set title = 'Hijack' where id = $1`, e); err != nil {
		t.Fatal(err)
	}
	if err := d.exec(owner, `update public.events set title = 'Renamed', live_state = 'live' where id = $1`, e); err != nil {
		t.Fatal(err)
	}
	if s := scalar[string](t, d, member, `select title from public.events where id = $1`, e); s != "Renamed" {
		t.Fatalf("title %q", s)
	}
	// Seat cap cannot drop below confirmed seats.
	wantCode(t, d.exec(owner, `update public.events set seat_cap = 0 where id = $1`, e), "23514")

	// Soft-deleted events disappear.
	d.su(t, `update public.events set deleted_at = now() where id = $1`, e)
	if n := scalar[int](t, d, member, `select count(*)::int from public.events`); n != 0 {
		t.Fatalf("member sees %d deleted events", n)
	}
	wantCode(t, d.rsvp(e, owner, "going", true, true), "PT404")
}

func TestSeatCap(t *testing.T) {
	d, owner, member, _, c := eventsFixture(t)
	e := d.newEvent(t, c, owner, 40, "3 days")
	// 37 of 40 confirmed: 3 seats left.
	for i := 0; i < 37; i++ {
		u := d.NewUser(t, fmt.Sprintf("Guest %d", i))
		if err := d.rsvp(e, u, "going", false, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := scalar[int](t, d, member, `select public.seats_left($1)`, e); n != 3 {
		t.Fatalf("seats left %d", n)
	}
	// Not sure / declined do not take seats.
	if err := d.rsvp(e, member, "not_sure", false, false); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, d, member, `select public.seats_left($1)`, e); n != 3 {
		t.Fatalf("not_sure took a seat: %d", n)
	}
	for i := 0; i < 3; i++ {
		u := d.NewUser(t, fmt.Sprintf("Late %d", i))
		if err := d.rsvp(e, u, "going", false, false); err != nil {
			t.Fatal(err)
		}
	}
	// 40 of 40: the next going RSVP is 'Event full'.
	wantCode(t, d.rsvp(e, member, "going", false, false), "PT409")
	if n := scalar[int](t, d, member, `select public.seats_left($1)`, e); n != 0 {
		t.Fatalf("seats left %d", n)
	}
	// A guest declining frees a seat.
	guest := mustService[string](t, d, `select user_id::text from public.rsvps where event_id = $1 and status = 'going' limit 1`, e)
	if err := d.rsvp(e, guest, "declined", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.rsvp(e, member, "going", false, false); err != nil {
		t.Fatalf("freed seat: %v", err)
	}
	if n := mustService[int](t, d, `select confirmed_count from public.events where id = $1`, e); n != 40 {
		t.Fatalf("confirmed_count %d", n)
	}
	// Deleting a confirmed RSVP also frees its seat.
	d.su(t, `delete from public.rsvps where event_id = $1 and user_id = $2`, e, member)
	if n := mustService[int](t, d, `select confirmed_count from public.events where id = $1`, e); n != 39 {
		t.Fatalf("confirmed_count after delete %d", n)
	}
}

func TestLastSeatRace(t *testing.T) {
	d, owner, _, _, c := eventsFixture(t)
	e := d.newEvent(t, c, owner, 1, "3 days")
	const n = 8
	users := make([]string, n)
	for i := range users {
		users[i] = d.NewUser(t, fmt.Sprintf("Racer %d", i))
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range users {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = d.rsvp(e, users[i], "going", false, false)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch SQLState(err) {
		case "":
			ok++
		case "PT409":
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d RSVPs confirmed for the last seat", ok)
	}
	if c := mustService[int](t, d, `select count(*)::int from public.rsvps where seat_confirmed`); c != 1 {
		t.Fatalf("%d confirmed rows", c)
	}
}

func TestPaidRsvpSeatNeedsVerification(t *testing.T) {
	d, owner, member, _, c := eventsFixture(t)
	e := mustService[string](t, d, `insert into public.events
		(community_id, creator_id, title, starts_at, ends_at, format, venue, seat_cap, is_paid, price_minor, payment_url)
		values ($1, $2, 'Paid', now() + interval '2 days', now() + interval '2 days 2 hours', 'in_person', 'Hall',
		        1, true, 49900, 'https://pay.example.com/p') returning id::text`, c, owner)
	if err := d.rsvp(e, member, "going", false, false); err != nil {
		t.Fatal(err)
	}
	if s := mustService[string](t, d, `select payment_status || ':' || seat_confirmed from public.rsvps where event_id = $1`, e); s != "pay_later:false" {
		t.Fatalf("paid going rsvp %s", s)
	}
	d.su(t, `update public.rsvps set payment_status = 'verified' where event_id = $1`, e)
	if s := mustService[bool](t, d, `select seat_confirmed from public.rsvps where event_id = $1`, e); !s {
		t.Fatal("verified payment did not confirm the seat")
	}
	other := d.NewUser(t, "Other")
	if err := d.rsvp(e, other, "going", false, false); err != nil {
		t.Fatalf("unverified going rsvp should not need a seat: %v", err)
	}
	if _, err := service[string](d, `update public.rsvps set payment_status = 'verified'
		where event_id = $1 and user_id = $2 returning id::text`, e, other); SQLState(err) != "PT409" {
		t.Fatalf("verify past cap: %v", err)
	}
}

func TestRsvpReminders(t *testing.T) {
	d, owner, member, _, c := eventsFixture(t)
	e := d.newEvent(t, c, owner, nil, "3 days")
	starts := mustService[time.Time](t, d, `select starts_at from public.events where id = $1`, e)

	count := func(channel string) int {
		return mustService[int](t, d, `select count(*)::int from public.reminders where event_id = $1 and user_id = $2 and channel = $3`, e, member, channel)
	}
	// Push on, email off: push 1 day and 1 hour before; no day-of email.
	if err := d.rsvp(e, member, "going", true, false); err != nil {
		t.Fatal(err)
	}
	if n := count("push"); n != 2 {
		t.Fatalf("%d push reminders", n)
	}
	if n := mustService[int](t, d, `select count(*)::int from public.reminders
		where event_id = $1 and channel = 'push' and send_at = $2::timestamptz - interval '1 hour'`, e, starts); n != 1 {
		t.Fatal("no push 1 hour before start")
	}
	if n := count("email"); n != 0 {
		t.Fatalf("%d email reminders with email off", n)
	}
	// Turn email on: one day-of email, on the event day (IST), before start.
	if err := d.rsvp(e, member, "going", true, true); err != nil {
		t.Fatal(err)
	}
	if ok := mustService[bool](t, d, `select bool_and((send_at at time zone 'Asia/Kolkata')::date = ($2::timestamptz at time zone 'Asia/Kolkata')::date
		and send_at < $2::timestamptz) and count(*) = 1 from public.reminders where event_id = $1 and channel = 'email'`, e, starts); !ok {
		t.Fatal("day-of email not scheduled on the event day")
	}
	// The member sees their reminders; the owner does not.
	if n := scalar[int](t, d, member, `select count(*)::int from public.reminders`); n != 3 {
		t.Fatalf("member sees %d reminders", n)
	}
	if n := scalar[int](t, d, owner, `select count(*)::int from public.reminders`); n != 0 {
		t.Fatalf("owner sees %d reminders", n)
	}
	// Rescheduling moves them with the start time.
	d.su(t, `update public.events set starts_at = starts_at + interval '1 day', ends_at = ends_at + interval '1 day' where id = $1`, e)
	if n := mustService[int](t, d, `select count(*)::int from public.reminders r join public.events e on e.id = r.event_id
		where r.event_id = $1 and r.channel = 'push' and r.send_at = e.starts_at - interval '1 hour'`, e); n != 1 {
		t.Fatal("push reminder not moved with the event")
	}
	if n := count("push") + count("email"); n != 3 {
		t.Fatalf("%d reminders after reschedule", n)
	}
	// Declining removes pending reminders.
	if err := d.rsvp(e, member, "declined", true, true); err != nil {
		t.Fatal(err)
	}
	if n := count("push") + count("email"); n != 0 {
		t.Fatalf("%d reminders after decline", n)
	}

	// "Remind me": a member inserts their own reminder for a visible event.
	if err := d.exec(member, `insert into public.reminders (event_id, channel, send_at) values ($1, 'push', now() + interval '1 day')`, e); err != nil {
		t.Fatal(err)
	}
	outsider := d.NewUser(t, "Out")
	wantCode(t, d.exec(outsider, `insert into public.reminders (event_id, channel, send_at) values ($1, 'push', now())`, e), "42501")
	wantCode(t, d.exec(member, `update public.reminders set sent_at = now()`), "42501")

	// The scheduler claims due reminders once.
	d.su(t, `insert into public.reminders (user_id, event_id, channel, send_at) values ($1, $2, 'push', now() - interval '1 minute')`, member, e)
	if n := mustService[int](t, d, `select count(*)::int from public.claim_due_reminders(10)`); n != 1 {
		t.Fatalf("claimed %d", n)
	}
	if n := mustService[int](t, d, `select count(*)::int from public.claim_due_reminders(10)`); n != 0 {
		t.Fatalf("claimed again %d", n)
	}
	wantCode(t, d.exec(member, `select * from public.claim_due_reminders(10)`), "42501")
}

func (d *DB) newRaise(t *testing.T, community, organiser, extraCols, extraVals string) (string, error) {
	t.Helper()
	return service[string](d, `insert into public.raises (community_id, organiser_id, title, receiver_name, payout_mode`+extraCols+`)
		values ($1, $2, 'Help fund', 'Asha Trust'`+extraVals+`) returning id::text`, community, organiser)
}

func TestRaiseValidation(t *testing.T) {
	d, owner, _, _, c := eventsFixture(t)
	bank := ", account_number, ifsc"
	check := func(cols, vals, code string) {
		t.Helper()
		_, err := d.newRaise(t, c, owner, cols, vals)
		wantCode(t, err, code)
	}
	check(", upi_id", ", 'upi', 'asha@okaxis'", "")
	check(bank, ", 'bank', '000123456789', 'hdfc0001234'", "")
	// Bank without IFSC, IFSC not 11 chars, UPI without UPI id.
	check(", account_number", ", 'bank', '000123456789'", "23514")
	check(bank, ", 'bank', '000123456789', 'HDFC000123'", "23514")
	check(bank, ", 'bank', '000123456789', 'HDFC00012345'", "22001")
	check("", ", 'upi'", "23514")
	// Negative goal, past open_until.
	check(", upi_id, goal_minor", ", 'upi', 'a@ok', -1", "23514")
	check(", upi_id, open_until", ", 'upi', 'a@ok', now() - interval '1 minute'", "23514")
	check(", upi_id, goal_minor, open_until", ", 'upi', 'a@ok', 0, now() + interval '7 days'", "")
	// Empty title.
	_, err := service[string](d, `insert into public.raises (community_id, organiser_id, title, receiver_name, payout_mode, upi_id)
		values ($1, $2, ' ', 'R', 'upi', 'a@ok') returning id::text`, c, owner)
	wantCode(t, err, "23514")

	// Server-set columns are derived.
	got := mustService[string](t, d, `select account_last4 || ':' || ifsc || ':' || raised_minor || ':' || donor_count
		from public.raises where payout_mode = 'bank'`)
	if got != "6789:HDFC0001234:0:0" {
		t.Fatalf("derived %s", got)
	}
}

func TestRaisePayoutMasking(t *testing.T) {
	d, owner, member, outsider, c := eventsFixture(t)
	organiser := d.NewUser(t, "Org")
	admin := d.NewUser(t, "Adm")
	d.addMember(t, c, organiser, "member")
	d.addMember(t, c, admin, "admin")
	r, err := d.newRaise(t, c, organiser, ", account_number, ifsc, qr_url",
		", 'bank', '000123456789', 'HDFC0001234', 'https://cdn.example.com/qr.png'")
	if err != nil {
		t.Fatal(err)
	}
	const q = `select coalesce(account_number, '-') || ':' || coalesce(ifsc, '-') || ':' || coalesce(qr_url, '-')
		|| ':' || account_last4 from public.raises_masked where id = $1`
	if s := scalar[string](t, d, member, q, r); s != "-:-:-:6789" {
		t.Fatalf("member sees %s", s)
	}
	for _, u := range []string{organiser, admin, owner} {
		if s := scalar[string](t, d, u, q, r); s != "000123456789:HDFC0001234:https://cdn.example.com/qr.png:6789" {
			t.Fatalf("%s sees %s", u, s)
		}
	}
	// Non-members and visitors see no raise at all.
	for _, u := range []string{outsider, ""} {
		if n := scalar[int](t, d, u, `select count(*)::int from public.raises_masked`); n != 0 {
			t.Fatalf("%q sees %d raises", u, n)
		}
	}
	// The base table is denied to every client role, even the organiser.
	for _, u := range []string{member, organiser, admin, ""} {
		wantCode(t, d.exec(u, `select account_number from public.raises`), "42501")
	}
	wantCode(t, d.exec(organiser, `update public.raises set title = 'x'`), "42501")
	wantCode(t, d.exec(member, `insert into public.raises (community_id, organiser_id, title, receiver_name, payout_mode, upi_id)
		values ($1, $2, 'x', 'r', 'upi', 'a@ok')`, c, member), "42501")
	// A filter on a masked column cannot probe the hidden value.
	if n := scalar[int](t, d, member, `select count(*)::int from public.raises_masked where account_number like '0001%'`); n != 0 {
		t.Fatalf("member probed account number: %d", n)
	}
}

func TestContributionsAreIdempotent(t *testing.T) {
	d, owner, member, _, c := eventsFixture(t)
	r, err := d.newRaise(t, c, owner, ", upi_id, goal_minor", ", 'upi', 'asha@okaxis', 1000000")
	if err != nil {
		t.Fatal(err)
	}
	capture := `select applied::text || ':' || contribution_id from public.capture_contribution($1, $2, $3, $4)`
	first := mustService[string](t, d, capture, r, member, 50000, "pay_A1")
	second := mustService[string](t, d, capture, r, member, 50000, "pay_A1")
	if first[:4] != "true" || second[:5] != "false" || first[5:] != second[6:] {
		t.Fatalf("first %s, second %s", first, second)
	}
	// Concurrent duplicates still apply once.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = service[string](d, capture, r, member, 25000, "pay_A2")
		}()
	}
	wg.Wait()
	if s := mustService[string](t, d, `select raised_minor || ':' || donor_count from public.raises where id = $1`, r); s != "75000:1" {
		t.Fatalf("raise totals %s", s)
	}
	// A created (pending) order captured later bumps once.
	d.su(t, `insert into public.contributions (raise_id, donor_id, amount_minor, gateway_payment_id) values ($1, $2, 100, 'pay_B')`, r, owner)
	mustService[string](t, d, capture, r, owner, 100, "pay_B")
	if s := mustService[string](t, d, `select raised_minor || ':' || donor_count from public.raises where id = $1`, r); s != "75100:2" {
		t.Fatalf("raise totals %s", s)
	}
	// Unique gateway_payment_id.
	_, err = service[string](d, `insert into public.contributions (raise_id, donor_id, amount_minor, gateway_payment_id)
		values ($1, $2, 1, 'pay_A1') returning id::text`, r, owner)
	wantCode(t, err, "23505")

	// Receipt marked once.
	id := first[5:]
	if !mustService[bool](t, d, `select public.mark_receipt_sent($1)`, id) {
		t.Fatal("receipt not marked")
	}
	if mustService[bool](t, d, `select public.mark_receipt_sent($1)`, id) {
		t.Fatal("receipt marked twice")
	}
	// Donor sees own contributions; another member does not; clients cannot write.
	if n := scalar[int](t, d, member, `select count(*)::int from public.contributions`); n != 2 {
		t.Fatalf("donor sees %d", n)
	}
	other := d.NewUser(t, "Other")
	d.addMember(t, c, other, "member")
	if n := scalar[int](t, d, other, `select count(*)::int from public.contributions`); n != 0 {
		t.Fatalf("other member sees %d", n)
	}
	if n := scalar[int](t, d, owner, `select count(*)::int from public.contributions`); n != 3 {
		t.Fatalf("organiser sees %d", n)
	}
	wantCode(t, d.exec(member, `select public.capture_contribution($1, $2, 1, 'x')`, r, member), "42501")
	wantCode(t, d.exec(member, `insert into public.contributions (raise_id, donor_id, amount_minor, gateway_payment_id)
		values ($1, $2, 1, 'y')`, r, member), "42501")
}
