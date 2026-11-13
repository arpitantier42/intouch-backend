package server

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/config"
	"github.com/intouch/api/internal/db"
)

type rsvpEnv struct {
	*sessEnv
	push *fakePush
}

func newRsvpEnv(t *testing.T) *rsvpEnv {
	e := newSessEnv(t)
	fp := &fakePush{}
	e.r = New(Deps{Config: config.Config{JWTSecret: secret}, DB: &db.DB{Pool: e.td.Pool}, Auth: e.fake, Push: fp, Mailer: e.mail})
	return &rsvpEnv{sessEnv: e, push: fp}
}

// rsvpUser is a joined member whose token carries the email claim.
func rsvpUser(t *testing.T, e *rsvpEnv, cid, email string) (string, string) {
	t.Helper()
	id, _ := admUser(t, e.sessEnv, email)
	chatJoin(t, e.sessEnv, cid, id)
	return id, emailToken(id, email)
}

func emailToken(id, email string) string {
	return auth.Sign(secret, map[string]any{"sub": id, "role": "authenticated", "aud": "authenticated",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "email": email,
		"session_id": "11111111-1111-1111-1111-111111111111"})
}

// newEvent inserts an event as the create-event RPC would. extra is
// ", col = val" pairs for paid events.
func newEvent(t *testing.T, e *rsvpEnv, cid, creator string, seatCap any, paid bool) string {
	t.Helper()
	var id string
	q := `insert into public.events (community_id, creator_id, title, starts_at, ends_at, format, join_url, seat_cap, is_paid, price_minor, payment_url)
	      values ($1, $2, 'Founders meetup', date_trunc('minute', now()) + interval '3 days', date_trunc('minute', now()) + interval '3 days 2 hours',
	              'online', 'https://meet.example.com/x', $3, $4,
	              case when $4 then 50000 end, case when $4 then 'https://pay.example.com/e' end)
	      returning id::text`
	if err := e.td.Pool.QueryRow(context.Background(), q, cid, creator, seatCap, paid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func reminders(t *testing.T, e *rsvpEnv, ev, uid string) map[string][]time.Time {
	t.Helper()
	rows, err := e.td.Pool.Query(context.Background(),
		`select channel, send_at from public.reminders where event_id = $1 and user_id = $2 and sent_at is null order by send_at`, ev, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]time.Time{}
	for rows.Next() {
		var ch string
		var at time.Time
		if err := rows.Scan(&ch, &at); err != nil {
			t.Fatal(err)
		}
		out[ch] = append(out[ch], at)
	}
	return out
}

func startsAt(t *testing.T, e *rsvpEnv, ev string) time.Time {
	t.Helper()
	var at time.Time
	if err := e.td.Pool.QueryRow(context.Background(), `select starts_at from public.events where id = $1`, ev).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func fillSeats(t *testing.T, e *rsvpEnv, ev string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		uid := e.fake.addUser(t, fmt.Sprintf("guest-%d-%d@example.com", time.Now().UnixNano(), i), "")
		if _, err := e.td.Pool.Exec(context.Background(), `select public.ensure_profile($1, 'Guest')`, uid); err != nil {
			t.Fatal(err)
		}
		if _, err := e.td.Pool.Exec(context.Background(),
			`insert into public.rsvps (event_id, user_id, status) values ($1, $2, 'going')`, ev, uid); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSetRsvpFreeEventRemindersAndConfirmation(t *testing.T) {
	e := newRsvpEnv(t)
	host, _ := admUser(t, e.sessEnv, "host@example.com")
	cid, _ := admCommunity(t, e.sessEnv, host, "Alpha", "private")
	ev := newEvent(t, e, cid, host, 40, false)
	asha, aTok := rsvpUser(t, e, cid, "asha@example.com")
	start := startsAt(t, e, ev)

	// Not joined → 403; unknown event → 404; bad status → 400.
	_, outsider := admUser(t, e.sessEnv, "out@example.com")
	if code, _ := e.raw(t, "PUT", "/events/"+ev+"/rsvp", outsider, map[string]any{"status": "going"}); code != 403 {
		t.Fatalf("outsider: %d", code)
	}
	if code, _ := e.raw(t, "PUT", "/events/00000000-0000-0000-0000-000000000000/rsvp", aTok, map[string]any{"status": "going"}); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
	if code, out := e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "maybe"}); code != 400 {
		t.Fatalf("bad status: %d %v", code, out)
	}

	// Free going: seat confirmed, join link, push 1 day and 1 hour before,
	// day-of email; confirmation email sent once.
	code, out := e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "going", "remind_push": true, "remind_email": true})
	if code != 200 || field(out, "seat_confirmed") != true || field(out, "badge") != "Going" {
		t.Fatalf("going: %d %v", code, out)
	}
	if field(out, "join_url") != "https://meet.example.com/x" || field(out, "seats_left_label") != "39 seats left" {
		t.Fatalf("join/seats: %v", out)
	}
	rem := reminders(t, e, ev, asha)
	if len(rem["push"]) != 2 || !rem["push"][1].Equal(start.Add(-time.Hour)) || !rem["push"][0].Equal(start.Add(-24*time.Hour)) {
		t.Fatalf("push reminders: %v (start %v)", rem["push"], start)
	}
	if len(rem["email"]) != 1 {
		t.Fatalf("email reminder: %v", rem["email"])
	}
	waitMail(t, e.mail, 1)
	if m := e.mail.sent[0]; m.To != "asha@example.com" || !strings.Contains(m.Subject, "You're in") || !strings.Contains(m.Text, "https://meet.example.com/x") {
		t.Fatalf("mail: %+v", m)
	}

	// Add to calendar (Google) carries the event start and end.
	cal := field(out, "calendar").(map[string]any)
	g, _ := url.Parse(cal["google"].(string))
	wantDates := start.UTC().Format("20060102T150405Z") + "/" + start.Add(2*time.Hour).UTC().Format("20060102T150405Z")
	if g.Host != "calendar.google.com" || g.Query().Get("dates") != wantDates || g.Query().Get("text") != "Founders meetup" {
		t.Fatalf("google calendar: %v want dates %s", g, wantDates)
	}
	if !strings.Contains(cal["ics"].(string), "DTSTART:"+start.UTC().Format("20060102T150405Z")) {
		t.Fatalf("ics: %v", cal["ics"])
	}

	// Repeating going does not re-send; turning email reminders off removes
	// the pending day-of email and keeps the pushes.
	code, out = e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "going", "remind_email": false})
	if code != 200 || field(out, "remind_email") != false || field(out, "remind_push") != true || field(out, "confirmation_email") != false {
		t.Fatalf("prefs: %d %v", code, out)
	}
	rem = reminders(t, e, ev, asha)
	if len(rem["email"]) != 0 || len(rem["push"]) != 2 {
		t.Fatalf("after email off: %v", rem)
	}
	time.Sleep(50 * time.Millisecond)
	if e.mail.count() != 1 {
		t.Fatalf("confirmation re-sent: %d", e.mail.count())
	}

	// Decline without a reason succeeds; the seat is freed, reminders go,
	// the host gets a quiet bell notification and no push.
	code, out = e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "declined"})
	if code != 200 || field(out, "badge") != "Declined" || field(out, "seat_confirmed") != false || field(out, "join_url") != nil {
		t.Fatalf("decline: %d %v", code, out)
	}
	if field(out, "seats_left") != float64(40) {
		t.Fatalf("seat not freed: %v", out)
	}
	if rem = reminders(t, e, ev, asha); len(rem) != 0 {
		t.Fatalf("reminders after decline: %v", rem)
	}
	var n int
	_ = e.td.Pool.QueryRow(context.Background(),
		`select count(*) from public.notifications where user_id = $1 and kind = 'rsvp_response'`, host).Scan(&n)
	if n != 1 {
		t.Fatalf("host notices: %d", n)
	}
	time.Sleep(50 * time.Millisecond)
	if e.push.count() != 0 {
		t.Fatalf("host was pushed on decline")
	}

	// Decline with a reason and note; unknown reason → 400.
	code, out = e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "declined", "decline_reason": "too_far", "note": "Next time!"})
	if code != 200 || field(out, "decline_reason") != "too_far" || field(out, "note") != "Next time!" {
		t.Fatalf("decline reason: %d %v", code, out)
	}
	if code, _ := e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "declined", "decline_reason": "bored"}); code != 400 {
		t.Fatalf("bad reason: %d", code)
	}
	// Not sure takes no seat.
	code, out = e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "not_sure"})
	if code != 200 || field(out, "badge") != "Not sure" || field(out, "decline_reason") != nil {
		t.Fatalf("not sure: %d %v", code, out)
	}
}

func waitMail(t *testing.T, m *fakeMailer, want int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if m.count() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d emails, got %d", want, m.count())
}

func TestSetRsvpSeatCap(t *testing.T) {
	e := newRsvpEnv(t)
	host, _ := admUser(t, e.sessEnv, "host@example.com")
	cid, _ := admCommunity(t, e.sessEnv, host, "Alpha", "private")
	ev := newEvent(t, e, cid, host, 40, false)
	_, aTok := rsvpUser(t, e, cid, "asha@example.com")

	// 37 of 40 confirmed → '3 seats left'.
	fillSeats(t, e, ev, 37)
	code, out := e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "not_sure"})
	if code != 200 || field(out, "seats_left_label") != "3 seats left" || field(out, "seats_left") != float64(3) {
		t.Fatalf("37/40: %d %v", code, out)
	}
	// 40 of 40 → 'Event full'.
	fillSeats(t, e, ev, 3)
	code, out = e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "going"})
	if code != 409 || !strings.Contains(fmt.Sprint(out), "Event full") {
		t.Fatalf("full: %d %v", code, out)
	}
}

func TestSetRsvpLastSeatRace(t *testing.T) {
	e := newRsvpEnv(t)
	host, _ := admUser(t, e.sessEnv, "host@example.com")
	cid, _ := admCommunity(t, e.sessEnv, host, "Alpha", "private")
	for round := 0; round < 5; round++ {
		ev := newEvent(t, e, cid, host, 2, false)
		fillSeats(t, e, ev, 1)
		var toks []string
		for i := 0; i < 2; i++ {
			_, tok := rsvpUser(t, e, cid, fmt.Sprintf("racer%d-%d@example.com", round, i))
			toks = append(toks, tok)
		}
		codes := make([]int, len(toks))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, tok := range toks {
			wg.Add(1)
			go func(i int, tok string) {
				defer wg.Done()
				<-start
				codes[i], _ = e.raw(t, "PUT", "/events/"+ev+"/rsvp", tok, map[string]any{"status": "going"})
			}(i, tok)
		}
		close(start)
		wg.Wait()
		ok, full := 0, 0
		for _, c := range codes {
			switch c {
			case 200:
				ok++
			case 409:
				full++
			}
		}
		var confirmed int
		_ = e.td.Pool.QueryRow(context.Background(), `select confirmed_count from public.events where id = $1`, ev).Scan(&confirmed)
		if ok != 1 || full != 1 || confirmed != 2 {
			t.Fatalf("round %d: codes %v confirmed %d", round, codes, confirmed)
		}
	}
}

func TestPaidRsvpAndVerification(t *testing.T) {
	e := newRsvpEnv(t)
	host, hTok := admUser(t, e.sessEnv, "host@example.com")
	cid, _ := admCommunity(t, e.sessEnv, host, "Alpha", "private")
	ev := newEvent(t, e, cid, host, 2, true)
	asha, aTok := rsvpUser(t, e, cid, "asha@example.com")
	bob, bTok := rsvpUser(t, e, cid, "bob@example.com")

	// I'll pay later: 'Payment pending', seat not confirmed, no join link.
	code, out := e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "going", "payment_status": "pay_later"})
	if code != 200 || field(out, "badge") != "Payment pending" || field(out, "seat_confirmed") != false || field(out, "join_url") != nil {
		t.Fatalf("pay later: %d %v", code, out)
	}
	// Going on a paid event without a choice defaults to pay_later.
	code, out = e.raw(t, "PUT", "/events/"+ev+"/rsvp", bTok, map[string]any{"status": "going"})
	if code != 200 || field(out, "payment_status") != "pay_later" {
		t.Fatalf("default pay later: %d %v", code, out)
	}
	// A member cannot verify themselves.
	if code, _ := e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "going", "payment_status": "verified"}); code != 400 {
		t.Fatalf("self verify: %d", code)
	}
	// I've paid: verification pending, and it stays so until the host acts.
	code, out = e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "going", "payment_status": "verification_pending"})
	if code != 200 || field(out, "badge") != "Verification pending" || field(out, "seat_confirmed") != false || field(out, "join_url") != nil {
		t.Fatalf("paid: %d %v", code, out)
	}
	code, out = e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "going", "remind_push": false})
	if code != 200 || field(out, "payment_status") != "verification_pending" {
		t.Fatalf("pending kept: %d %v", code, out)
	}

	// Only the creator verifies.
	if code, _ := e.raw(t, "POST", "/events/"+ev+"/rsvps/"+asha+"/verify", bTok, nil); code != 403 {
		t.Fatalf("non-host verify: %d", code)
	}
	if code, _ := e.raw(t, "POST", "/events/"+ev+"/rsvps/"+host+"/verify", hTok, nil); code != 404 {
		t.Fatalf("no rsvp: %d", code)
	}
	code, out = e.raw(t, "POST", "/events/"+ev+"/rsvps/"+asha+"/verify", hTok, nil)
	if code != 200 || field(out, "payment_status") != "verified" || field(out, "seat_confirmed") != true || field(out, "badge") != "Going" {
		t.Fatalf("verify: %d %v", code, out)
	}
	if n := e.push.last(t, 1); n.UserIDs[0] != asha {
		t.Fatalf("push: %+v", n)
	}
	// Idempotent; the attendee now sees the join link.
	if code, _ := e.raw(t, "POST", "/events/"+ev+"/rsvps/"+asha+"/verify", hTok, nil); code != 200 {
		t.Fatalf("re-verify: %d", code)
	}
	code, out = e.raw(t, "PUT", "/events/"+ev+"/rsvp", aTok, map[string]any{"status": "going"})
	if code != 200 || field(out, "join_url") != "https://meet.example.com/x" || field(out, "payment_status") != "verified" {
		t.Fatalf("after verify: %d %v", code, out)
	}

	// Cap reached (1 of 1 confirmed): verifying bob is 'Event full'.
	if _, err := e.td.Pool.Exec(context.Background(), `update public.events set seat_cap = 1 where id = $1`, ev); err != nil {
		t.Fatal(err)
	}
	if code, out := e.raw(t, "POST", "/events/"+ev+"/rsvps/"+bob+"/verify", hTok, nil); code != 409 {
		t.Fatalf("verify when full: %d %v", code, out)
	}
}
