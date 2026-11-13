package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/intouch/api/internal/db"
	"github.com/intouch/api/internal/mailer"
)

// invite issues invitations and returns the status and body.
func (e *sessEnv) invite(t *testing.T, cid, tok string, body any) (int, any) {
	t.Helper()
	return e.raw(t, "POST", "/communities/"+cid+"/invitations", tok, body)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestInvitationPermissions(t *testing.T) {
	e := newSessEnv(t)
	owner, ownerTok := admUser(t, e, "owner@example.com")
	mod, modTok := admUser(t, e, "mod@example.com")
	_, outsiderTok := admUser(t, e, "outsider@example.com")
	cid, _ := admCommunity(t, e, owner, "Gophers", "private")
	ctx := context.Background()
	if _, err := e.td.Pool.Exec(ctx, `insert into public.memberships (community_id, user_id, role, status, via) values ($1, $2, 'moderator', 'joined', 'direct')`, cid, mod); err != nil {
		t.Fatal(err)
	}
	link := map[string]any{"channel": "link"}

	// Default invite_permission = admins: a moderator and an outsider get 403.
	if code, out := e.invite(t, cid, modTok, link); code != 403 {
		t.Fatalf("moderator default: %d %v", code, out)
	}
	if code, _ := e.invite(t, cid, outsiderTok, link); code != 403 {
		t.Fatalf("outsider: %d", code)
	}
	code, out := e.invite(t, cid, ownerTok, link)
	if code != 201 || field(out, "channel") != "link" || len(field(out, "code").(string)) != 32 || field(out, "tracking_state") != "sent" {
		t.Fatalf("owner link: %d %v", code, out)
	}
	ownerCode := field(out, "code").(string)

	// The moderator cannot revoke either.
	if code, _ := e.raw(t, "DELETE", "/communities/"+cid+"/invitations/"+ownerCode, modTok, nil); code != 403 {
		t.Fatalf("moderator revoke: %d", code)
	}

	// All members: the moderator (any joined role) can invite.
	if _, err := e.td.Pool.Exec(ctx, `update public.communities set invite_permission = 'all_members' where id = $1`, cid); err != nil {
		t.Fatal(err)
	}
	if code, out := e.invite(t, cid, modTok, map[string]any{"channel": "whatsapp", "max_uses": 5}); code != 201 || field(out, "max_uses") != float64(5) {
		t.Fatalf("all members: %d %v", code, out)
	}

	// Validation: bad channel, past expiry, max_uses 0.
	if code, _ := e.invite(t, cid, ownerTok, map[string]any{"channel": "fax"}); code != 400 {
		t.Fatalf("channel: %d", code)
	}
	if code, _ := e.invite(t, cid, ownerTok, map[string]any{"channel": "link", "expires_at": time.Now().Add(-time.Hour)}); code != 400 {
		t.Fatalf("past expiry: %d", code)
	}
	if code, _ := e.invite(t, cid, ownerTok, map[string]any{"channel": "link", "max_uses": 0}); code != 400 {
		t.Fatalf("max uses: %d", code)
	}
}

func TestInvitationInAppAndEmail(t *testing.T) {
	e := newSessEnv(t)
	owner, ownerTok := admUser(t, e, "owner@example.com")
	a, _ := admUser(t, e, "a@example.com")
	b, _ := admUser(t, e, "b@example.com")
	member, memberTok := admUser(t, e, "member@example.com")
	cid, qid := admCommunity(t, e, owner, "Gophers", "private")

	// Make member joined through an invitation.
	_, out := e.invite(t, cid, ownerTok, map[string]any{"channel": "in_app", "invitee_user_id": member})
	code, out := e.raw(t, "POST", "/communities/"+cid+"/invitations/"+field(out, "code").(string)+"/accept", memberTok,
		[]map[string]string{{"question_id": qid, "value": "hi"}})
	if code != 200 {
		t.Fatalf("member accept: %d %v", code, out)
	}

	// Two in-app users → two invitations with distinct codes.
	code, out = e.invite(t, cid, ownerTok, map[string]any{"channel": "in_app", "invitee_user_ids": []string{a, b}})
	list, _ := out.([]any)
	if code != 201 || len(list) != 2 || field(list[0], "code") == field(list[1], "code") {
		t.Fatalf("two users: %d %v", code, out)
	}
	// Self and existing members are rejected.
	if code, _ := e.invite(t, cid, ownerTok, map[string]any{"channel": "in_app", "invitee_user_ids": []string{owner}}); code != 400 {
		t.Fatalf("self: %d", code)
	}
	if code, _ := e.invite(t, cid, ownerTok, map[string]any{"channel": "in_app", "invitee_user_ids": []string{a, member}}); code != 400 {
		t.Fatalf("member: %d", code)
	}

	// One malformed email: flagged, nothing created or sent.
	var before int
	e.td.Pool.QueryRow(context.Background(), `select count(*) from public.invitations`).Scan(&before)
	code, out = e.invite(t, cid, ownerTok, map[string]any{"channel": "email", "emails": []string{"new@example.com", "bad@@x"}})
	fields, _ := field(field(out, "error"), "fields").(map[string]any)
	if code != 400 || fields["emails[1]"] == nil || fields["emails[0]"] != nil {
		t.Fatalf("malformed: %d %v", code, out)
	}
	var after int
	e.td.Pool.QueryRow(context.Background(), `select count(*) from public.invitations`).Scan(&after)
	if after != before || e.mail.count() != 0 {
		t.Fatalf("malformed batch created %d invitations, sent %d", after-before, e.mail.count())
	}
	// The member's email is an existing member.
	if code, _ := e.invite(t, cid, ownerTok, map[string]any{"channel": "email", "emails": []string{"Member@example.com"}}); code != 400 {
		t.Fatalf("member email: %d", code)
	}

	// Valid batch: the platform sends one email per address with the link.
	code, out = e.invite(t, cid, ownerTok, map[string]any{"channel": "email", "emails": []string{"x@example.com", "y@example.com"}, "note": "Join us!"})
	list, _ = out.([]any)
	if code != 201 || len(list) != 2 {
		t.Fatalf("emails: %d %v", code, out)
	}
	waitFor(t, "invite emails", func() bool { return e.mail.count() == 2 })
	e.mail.mu.Lock()
	msg := e.mail.sent[0]
	e.mail.mu.Unlock()
	if !strings.Contains(msg.Text, "communities/"+cid+"/invite/") || !strings.Contains(msg.Text, "Join us!") || !strings.Contains(msg.Subject, "Gophers") {
		t.Fatalf("email: %+v", msg)
	}
}

// failingMailer fails every send.
type failingMailer struct {
	mu sync.Mutex
	n  int
}

func (m *failingMailer) Send(context.Context, mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	return errors.New("bounced")
}

func TestInvitationEmailFailureMarksFailed(t *testing.T) {
	e := newSessEnv(t)
	owner, ownerTok := admUser(t, e, "owner@example.com")
	cid, _ := admCommunity(t, e, owner, "Gophers", "private")
	fm := &failingMailer{}
	h := &invitationHandlers{d: Deps{DB: &db.DB{Pool: e.td.Pool}, Mailer: fm}}

	code, out := e.invite(t, cid, ownerTok, map[string]any{"channel": "email", "invitee_email": "x@example.com"})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	email := "x@example.com"
	h.sendInviteEmails(cid, "Gophers", "Owner", "", []issuedInvitation{{ID: field(out, "id").(string), Code: field(out, "code").(string), InviteeEmail: &email}})
	var state string
	e.td.Pool.QueryRow(context.Background(), `select tracking_state from public.invitations where id = $1`, field(out, "id")).Scan(&state)
	if state != "failed" {
		t.Fatalf("state = %q", state)
	}
}

func TestInvitationOpenAcceptRevoke(t *testing.T) {
	e := newSessEnv(t)
	owner, ownerTok := admUser(t, e, "owner@example.com")
	_, joinerTok := admUser(t, e, "joiner@example.com")
	_, secondTok := admUser(t, e, "second@example.com")
	cid, qid := admCommunity(t, e, owner, "Gophers", "private")
	other, _ := admCommunity(t, e, owner, "Other", "private")
	ctx := context.Background()

	_, out := e.invite(t, cid, ownerTok, map[string]any{"channel": "link", "max_uses": 1})
	inv := field(out, "code").(string)
	base := "/communities/" + cid + "/invitations/" + inv

	// Public open: preview + questions, tracking becomes opened.
	code, out := e.raw(t, "GET", base, "", nil)
	if code != 200 || field(out, "tracking_state") != "opened" || field(field(out, "community"), "name") != "Gophers" {
		t.Fatalf("open: %d %v", code, out)
	}
	if qs, _ := field(out, "questions").([]any); len(qs) != 1 {
		t.Fatalf("questions: %v", out)
	}
	// Wrong community id or unknown code: 404 with the not-valid message.
	for _, p := range []string{"/communities/" + other + "/invitations/" + inv, "/communities/" + cid + "/invitations/nope", "/communities/not-a-uuid/invitations/" + inv} {
		code, out := e.raw(t, "GET", p, "", nil)
		if code != 404 || field(field(out, "error"), "message") != invalidInviteMsg {
			t.Fatalf("invalid %s: %d %v", p, code, out)
		}
	}
	if code, _ := e.raw(t, "POST", "/communities/"+other+"/invitations/"+inv+"/accept", joinerTok, nil); code != 404 {
		t.Fatalf("accept other community: %d", code)
	}

	// Accept: blank required answer → 400; then joined via invite, no request.
	if code, _ := e.raw(t, "POST", base+"/accept", joinerTok, []map[string]string{{"question_id": qid, "value": " "}}); code != 400 {
		t.Fatalf("blank answer: %d", code)
	}
	code, out = e.raw(t, "POST", base+"/accept", joinerTok, []map[string]string{{"question_id": qid, "value": "hi"}})
	if code != 200 || field(out, "status") != "joined" || field(out, "via") != "invite" || field(out, "invitation_id") == nil {
		t.Fatalf("accept: %d %v", code, out)
	}
	var pending int
	e.td.Pool.QueryRow(ctx, `select count(*) from public.memberships where community_id = $1 and status = 'pending'`, cid).Scan(&pending)
	if pending != 0 {
		t.Fatalf("pending requests: %d", pending)
	}

	// Max uses reached: the next acceptance is 410 and the landing is invalid.
	if code, _ := e.raw(t, "POST", base+"/accept", secondTok, []map[string]string{{"question_id": qid, "value": "hi"}}); code != 410 {
		t.Fatalf("used up: %d", code)
	}
	if code, _ := e.raw(t, "GET", base, "", nil); code != 404 {
		t.Fatalf("used up landing: %d", code)
	}

	// Existing member accepting another invite: unchanged, no duplicate.
	_, out = e.invite(t, cid, ownerTok, map[string]any{"channel": "link", "max_uses": 3})
	inv2 := "/communities/" + cid + "/invitations/" + field(out, "code").(string)
	if code, _ := e.raw(t, "POST", inv2+"/accept", joinerTok, nil); code != 200 {
		t.Fatalf("re-accept: %d", code)
	}
	var rows, uses int
	e.td.Pool.QueryRow(ctx, `select count(*) from public.memberships m join auth.users u on u.id = m.user_id where m.community_id = $1 and u.email = 'joiner@example.com'`, cid).Scan(&rows)
	e.td.Pool.QueryRow(ctx, `select use_count from public.invitations where code = $1`, field(out, "code")).Scan(&uses)
	if rows != 1 || uses != 0 {
		t.Fatalf("re-accept rows=%d uses=%d", rows, uses)
	}

	// Revoked: 204, then accept 410 and landing 404.
	if code, out := e.raw(t, "DELETE", inv2, ownerTok, nil); code != 204 {
		t.Fatalf("revoke: %d %v", code, out)
	}
	if code, _ := e.raw(t, "POST", inv2+"/accept", secondTok, nil); code != 410 {
		t.Fatalf("revoked accept: %d", code)
	}
	if code, _ := e.raw(t, "GET", inv2, "", nil); code != 404 {
		t.Fatalf("revoked landing: %d", code)
	}

	// Expired (e.g. during sign-up): accept 410, landing 404.
	_, out = e.invite(t, cid, ownerTok, map[string]any{"channel": "link"})
	if _, err := e.td.Pool.Exec(ctx, `update public.invitations set expires_at = now() - interval '1 second' where code = $1`, field(out, "code")); err != nil {
		t.Fatal(err)
	}
	inv3 := "/communities/" + cid + "/invitations/" + field(out, "code").(string)
	if code, _ := e.raw(t, "POST", inv3+"/accept", secondTok, nil); code != 410 {
		t.Fatalf("expired accept: %d", code)
	}
	code, out = e.raw(t, "GET", inv3, "", nil)
	if code != 404 || field(field(out, "error"), "message") != invalidInviteMsg {
		t.Fatalf("expired landing: %d %v", code, out)
	}
}

func TestInvitationCodesAreUnique(t *testing.T) {
	e := newSessEnv(t)
	owner, _ := admUser(t, e, "owner@example.com")
	cid, _ := admCommunity(t, e, owner, "Gophers", "private")
	ctx := context.Background()
	if _, err := e.td.Pool.Exec(ctx, `insert into public.invitations (community_id, channel, created_by)
		select $1, 'link', $2 from generate_series(1, 10000)`, cid, owner); err != nil {
		t.Fatal(err)
	}
	var n, distinct int
	if err := e.td.Pool.QueryRow(ctx, `select count(*), count(distinct code) from public.invitations where code ~ '^[0-9a-f]{32}$'`).Scan(&n, &distinct); err != nil {
		t.Fatal(err)
	}
	if n != 10000 || distinct != n {
		t.Fatalf("codes: %d rows, %d distinct", n, distinct)
	}
}
