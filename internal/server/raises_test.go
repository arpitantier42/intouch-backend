package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/intouch/api/internal/config"
	"github.com/intouch/api/internal/db"
	"github.com/intouch/api/internal/dbtest"
	"github.com/intouch/api/internal/payments"
	"github.com/jackc/pgx/v5"
)

type fakeGateway struct {
	mu     sync.Mutex
	orders []payments.Order
	fail   bool
}

func (g *fakeGateway) Provider() string { return "razorpay" }

func (g *fakeGateway) CreateOrder(_ context.Context, o payments.Order) (payments.CreatedOrder, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fail {
		return payments.CreatedOrder{}, errors.New("down")
	}
	g.orders = append(g.orders, o)
	return payments.CreatedOrder{ID: "order_" + strings.Repeat("x", 4) + string(rune('a'+len(g.orders))),
		AmountMinor: o.AmountMinor, Currency: o.Currency, KeyID: "rzp_test_key"}, nil
}

const (
	testAPIKey     = "internal-key"
	razorpaySecret = "rzp-webhook-secret"
	emailSecret    = "email-webhook-secret"
)

// newRaiseEnv builds the router; gw nil = no gateway configured.
func newRaiseEnv(t *testing.T, gw payments.Gateway) *sessEnv {
	e := newSessEnv(t)
	d := Deps{Config: config.Config{JWTSecret: secret, WebhookAPIKey: testAPIKey,
		RazorpayWebhook: razorpaySecret, EmailWebhook: emailSecret},
		DB: &db.DB{Pool: e.td.Pool}, Auth: e.fake, Mailer: e.mail}
	if gw != nil {
		d.Payments = gw
	}
	e.r = New(d)
	return e
}

func raiseBodyBank() map[string]any {
	return map[string]any{"title": "Ward 7 library", "goal_minor": 10000000, "receiver_name": "Asha Rao",
		"payout_mode": "bank", "account_number": "123456789012", "ifsc": "hdfc0001234",
		"open_until": time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)}
}

func createRaise(t *testing.T, e *sessEnv, tok, cid string, body map[string]any) map[string]any {
	t.Helper()
	code, out := e.raw(t, "POST", "/communities/"+cid+"/raises", tok, body)
	if code != 201 {
		t.Fatalf("create raise: %d %v", code, out)
	}
	return out.(map[string]any)
}

func (e *sessEnv) webhook(t *testing.T, provider, sigHeader, sig, key string, body []byte) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/internal/webhooks/"+provider, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", key)
	if sigHeader != "" {
		req.Header.Set(sigHeader, sig)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func capturedBody(orderID, paymentID string, amount int64) []byte {
	b, _ := json.Marshal(map[string]any{"event": "payment.captured", "payload": map[string]any{
		"payment": map[string]any{"entity": map[string]any{"id": paymentID, "order_id": orderID,
			"amount": amount, "currency": "INR", "status": "captured"}}}})
	return b
}

func TestCreateRaiseValidation(t *testing.T) {
	e := newRaiseEnv(t, nil)
	host, hTok := admUser(t, e, "host@example.com")
	cid, _ := admCommunity(t, e, host, "Alpha", "private")

	for name, tc := range map[string]struct {
		mut   func(b map[string]any)
		field string
		msg   string
	}{
		"empty title":    {func(b map[string]any) { b["title"] = "  " }, "title", "Add a title"},
		"negative goal":  {func(b map[string]any) { b["goal_minor"] = -100 }, "goal_minor", "Goal can't be negative"},
		"past deadline":  {func(b map[string]any) { b["open_until"] = time.Now().Add(-time.Hour).Format(time.RFC3339) }, "open_until", "Open until must be in the future"},
		"bank no ifsc":   {func(b map[string]any) { delete(b, "ifsc") }, "ifsc", "Add the IFSC code"},
		"ifsc 10 chars":  {func(b map[string]any) { b["ifsc"] = "HDFC000123" }, "ifsc", "IFSC must be 11 characters"},
		"ifsc 12 chars":  {func(b map[string]any) { b["ifsc"] = "HDFC00012345" }, "ifsc", "IFSC must be 11 characters"},
		"no receiver":    {func(b map[string]any) { delete(b, "receiver_name") }, "receiver_name", "Add who the money goes to"},
		"upi without id": {func(b map[string]any) { b["payout_mode"] = "upi" }, "upi_id", "Add a UPI ID"},
	} {
		b := raiseBodyBank()
		tc.mut(b)
		code, out := e.raw(t, "POST", "/communities/"+cid+"/raises", hTok, b)
		errObj, _ := field(out, "error").(map[string]any)
		fields, _ := errObj["fields"].(map[string]any)
		if code != 400 || fields[tc.field] != tc.msg {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	var n int
	_ = e.td.Pool.QueryRow(context.Background(), `select count(*) from public.raises`).Scan(&n)
	if n != 0 {
		t.Fatalf("rejected raises were stored: %d", n)
	}

	// The RPC enforces the same rules when called directly.
	for name, sql := range map[string]string{
		"empty title":   `select public.create_raise($1, ' ', null, null, null, null, false, 'A', 'upi', 'a@okhdfc')`,
		"negative goal": `select public.create_raise($1, 'T', null, -1, null, null, false, 'A', 'upi', 'a@okhdfc')`,
		"past":          `select public.create_raise($1, 'T', null, null, now() - interval '1 hour', null, false, 'A', 'upi', 'a@okhdfc')`,
		"bank no ifsc":  `select public.create_raise($1, 'T', null, null, null, null, false, 'A', 'bank', null, '123456789')`,
		"ifsc short":    `select public.create_raise($1, 'T', null, null, null, null, false, 'A', 'bank', null, '123456789', 'HDFC01')`,
	} {
		code, _ := e.rpcAs(t, host, sql, cid)
		if code != "PT400" {
			t.Errorf("rpc %s: %s", name, code)
		}
	}

	// A non-member cannot create one.
	_, oTok := admUser(t, e, "outsider@example.com")
	if code, out := e.raw(t, "POST", "/communities/"+cid+"/raises", oTok, raiseBodyBank()); code != 403 {
		t.Fatalf("outsider: %d %v", code, out)
	}
}

// rpcAs runs sql as the user and returns the SQLSTATE ("" on success).
func (e *sessEnv) rpcAs(t *testing.T, uid, sql string, args ...any) (string, error) {
	t.Helper()
	err := e.td.As(uid, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	})
	return dbtest.SQLState(err), err
}

func TestRaiseCreateReadMaskAndClose(t *testing.T) {
	e := newRaiseEnv(t, nil)
	host, hTok := admUser(t, e, "host@example.com")
	cid, _ := admCommunity(t, e, host, "Alpha", "private")
	org, orgTok := admUser(t, e, "org@example.com")
	chatJoin(t, e, cid, org)
	mem, mTok := admUser(t, e, "mem@example.com")
	chatJoin(t, e, cid, mem)
	_, oTok := admUser(t, e, "outsider@example.com")

	r := createRaise(t, e, orgTok, cid, raiseBodyBank())
	id := r["id"].(string)
	if r["ifsc"] != "HDFC0001234" || r["account_number"] != "123456789012" || r["account_last4"] != "9012" || r["post_id"] == nil {
		t.Fatalf("organiser view: %v", r)
	}
	// Feed post added.
	var kind, body string
	if err := e.td.Pool.QueryRow(context.Background(),
		`select p.kind, p.body from public.posts p join public.post_targets t on t.post_id = p.id
		 where p.raise_id = $1 and t.community_id = $2`, id, cid).Scan(&kind, &body); err != nil || kind != "raise" || body != "Ward 7 library" {
		t.Fatalf("feed post: %v %s %s", err, kind, body)
	}

	// Member sees only the last 4 digits; the JSON carries no unmasked values.
	code, out := e.raw(t, "GET", "/raises/"+id, mTok, nil)
	m := out.(map[string]any)
	if code != 200 || m["account_number"] != nil || m["ifsc"] != nil || m["upi_id"] != nil || m["qr_url"] != nil ||
		m["account_last4"] != "9012" || m["payout_visible"] != false {
		t.Fatalf("member view: %d %v", code, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "123456789012") || strings.Contains(string(raw), "HDFC0001234") {
		t.Fatalf("member response leaks payout: %s", raw)
	}
	// Owner (admin role) sees full details.
	if _, out := e.raw(t, "GET", "/raises/"+id, hTok, nil); field(out, "account_number") != "123456789012" {
		t.Fatalf("owner view: %v", out)
	}
	// Non-member and unknown → 404.
	if code, _ := e.raw(t, "GET", "/raises/"+id, oTok, nil); code != 404 {
		t.Fatalf("outsider: %d", code)
	}
	if code, _ := e.raw(t, "GET", "/raises/00000000-0000-0000-0000-000000000000", mTok, nil); code != 404 {
		t.Fatalf("unknown: %d", code)
	}

	// Progress: ₹25,000 of ₹1,00,000 goal.
	for i, amt := range []int64{1500000, 1000000} {
		donor := mem
		if i == 1 {
			donor = host
		}
		if _, err := e.td.Pool.Exec(context.Background(),
			`select * from public.capture_contribution($1, $2, $3, $4)`, id, donor, amt, "pay_"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	_, out = e.raw(t, "GET", "/raises/"+id, mTok, nil)
	if field(out, "progress_label") != "₹25,000 of ₹1,00,000 goal" || field(out, "progress_percent") != float64(25) ||
		field(out, "donor_count") != float64(2) {
		t.Fatalf("progress: %v", out)
	}
	// Over the goal the bar caps at 100%.
	if _, err := e.td.Pool.Exec(context.Background(),
		`select * from public.capture_contribution($1, $2, $3, 'pay_big')`, id, mem, int64(20000000)); err != nil {
		t.Fatal(err)
	}
	_, out = e.raw(t, "GET", "/raises/"+id, mTok, nil)
	if field(out, "progress_percent") != float64(100) || field(out, "progress_label") != "₹2,25,000 of ₹1,00,000 goal" {
		t.Fatalf("cap: %v", out)
	}
	// No gateway: Support shows the unavailable message.
	sup, _ := field(out, "support").(map[string]any)
	if sup["enabled"] != false || sup["message"] != paymentsUnavailableMsg {
		t.Fatalf("support: %v", sup)
	}

	// Close: non-organiser (even the owner) 403; organiser 200.
	if code, _ := e.raw(t, "POST", "/raises/"+id+"/close", mTok, nil); code != 403 {
		t.Fatalf("member close: %d", code)
	}
	if code, _ := e.raw(t, "POST", "/raises/"+id+"/close", hTok, nil); code != 403 {
		t.Fatalf("owner close: %d", code)
	}
	code, out = e.raw(t, "POST", "/raises/"+id+"/close", orgTok, nil)
	if code != 200 || field(out, "closed_at") == nil || field(out, "status") != "closed" || field(out, "is_open") != false {
		t.Fatalf("close: %d %v", code, out)
	}
	if code, out := e.raw(t, "POST", "/raises/"+id+"/contributions", mTok, map[string]any{"amount_minor": 10000}); code != 409 {
		t.Fatalf("contribute to closed: %d %v", code, out)
	}
	_ = org
}

func TestContributionsWithoutGateway(t *testing.T) {
	e := newRaiseEnv(t, nil)
	host, hTok := admUser(t, e, "host@example.com")
	cid, _ := admCommunity(t, e, host, "Alpha", "private")
	b := raiseBodyBank()
	b["payout_mode"], b["upi_id"] = "upi", "asha@okhdfc"
	r := createRaise(t, e, hTok, cid, b)
	code, out := e.raw(t, "POST", "/raises/"+r["id"].(string)+"/contributions", hTok, map[string]any{"amount_minor": 50000})
	if code != 503 || field(field(out, "error"), "message") != paymentsUnavailableMsg {
		t.Fatalf("no gateway: %d %v", code, out)
	}
	if r["account_number"] != nil || r["upi_id"] != "asha@okhdfc" {
		t.Fatalf("upi raise: %v", r)
	}
}

func TestContributionGatewayWebhookAndReceipt(t *testing.T) {
	gw := &fakeGateway{}
	e := newRaiseEnv(t, gw)
	host, hTok := admUser(t, e, "host@example.com")
	cid, _ := admCommunity(t, e, host, "Alpha", "private")
	donor, dTok := admUser(t, e, "donor@example.com")
	chatJoin(t, e, cid, donor)
	r := createRaise(t, e, hTok, cid, raiseBodyBank())
	id := r["id"].(string)
	if sup, _ := r["support"].(map[string]any); sup["mode"] != "gateway" || sup["enabled"] != true {
		t.Fatalf("support with gateway: %v", r["support"])
	}

	if code, _ := e.raw(t, "POST", "/raises/"+id+"/contributions", dTok, map[string]any{"amount_minor": 0}); code != 400 {
		t.Fatalf("zero amount: %d", code)
	}
	code, out := e.raw(t, "POST", "/raises/"+id+"/contributions", dTok, map[string]any{"amount_minor": 250000})
	if code != 201 || field(out, "status") != "created" || field(field(out, "checkout"), "key_id") != "rzp_test_key" {
		t.Fatalf("contribute: %d %v", code, out)
	}
	order := field(out, "gateway_payment_id").(string)
	if gw.orders[0].Notes["raise_id"] != id || gw.orders[0].Notes["donor_id"] != donor {
		t.Fatalf("order notes: %v", gw.orders[0])
	}

	body := capturedBody(order, "pay_123", 250000)
	// Bad signature or key → 401.
	if code, _ := e.webhook(t, "razorpay", "X-Razorpay-Signature", "deadbeef", testAPIKey, body); code != 401 {
		t.Fatalf("bad signature: %d", code)
	}
	if code, _ := e.webhook(t, "razorpay", "X-Razorpay-Signature", payments.Sign(razorpaySecret, body), "nope", body); code != 401 {
		t.Fatalf("bad key: %d", code)
	}
	sig := payments.Sign(razorpaySecret, body)
	code, res := e.webhook(t, "razorpay", "X-Razorpay-Signature", sig, testAPIKey, body)
	if code != 200 || res["status"] != "applied" {
		t.Fatalf("first webhook: %d %v", code, res)
	}
	code, res = e.webhook(t, "razorpay", "X-Razorpay-Signature", sig, testAPIKey, body)
	if code != 200 || res["status"] != "duplicate" {
		t.Fatalf("second webhook: %d %v", code, res)
	}
	var raised int64
	var donors int
	_ = e.td.Pool.QueryRow(context.Background(), `select raised_minor, donor_count from public.raises where id = $1`, id).Scan(&raised, &donors)
	if raised != 250000 || donors != 1 {
		t.Fatalf("totals: %d %d", raised, donors)
	}
	// Receipt emailed once to the donor and recorded.
	waitFor(t, "receipt", func() bool {
		var sent bool
		_ = e.td.Pool.QueryRow(context.Background(),
			`select receipt_sent_at is not null from public.contributions where gateway_payment_id = $1`, order).Scan(&sent)
		return sent
	})
	e.mail.mu.Lock()
	defer e.mail.mu.Unlock()
	if len(e.mail.sent) != 1 || e.mail.sent[0].To != "donor@example.com" || !strings.Contains(e.mail.sent[0].Text, "₹2,500") {
		t.Fatalf("receipt mail: %+v", e.mail.sent)
	}

	// Other events are acknowledged and ignored.
	other := []byte(`{"event":"payment.failed","payload":{"payment":{"entity":{"id":"pay_9","status":"failed"}}}}`)
	if code, res := e.webhook(t, "razorpay", "X-Razorpay-Signature", payments.Sign(razorpaySecret, other), testAPIKey, other); code != 200 || res["status"] != "ignored" {
		t.Fatalf("ignored: %d %v", code, res)
	}
}

func TestEmailBounceMarksInvitationFailed(t *testing.T) {
	e := newRaiseEnv(t, nil)
	host, hTok := admUser(t, e, "host@example.com")
	cid, _ := admCommunity(t, e, host, "Alpha", "private")
	code, out := e.invite(t, cid, hTok, map[string]any{"channel": "email", "emails": []string{"a@example.com", "b@example.com"}})
	if code != 201 {
		t.Fatalf("invite: %d %v", code, out)
	}
	invs := out.([]any)
	idA := field(invs[0], "id").(string)
	waitFor(t, "invite mail", func() bool { return e.mail.count() == 2 })

	state := func(email string) string {
		var s string
		_ = e.td.Pool.QueryRow(context.Background(), `select tracking_state from public.invitations where invitee_email = $1`, email).Scan(&s)
		return s
	}
	byID, _ := json.Marshal(map[string]any{"type": "bounce", "metadata": map[string]string{"invitation_id": idA}})
	if code, _ := e.webhook(t, "email", "X-Webhook-Signature", "bad", testAPIKey, byID); code != 401 {
		t.Fatalf("bad signature: %d", code)
	}
	if code, res := e.webhook(t, "email", "X-Webhook-Signature", payments.Sign(emailSecret, byID), testAPIKey, byID); code != 200 || res["marked"] != float64(1) {
		t.Fatalf("bounce by id: %d %v", code, res)
	}
	if state("a@example.com") != "failed" || state("b@example.com") != "sent" {
		t.Fatalf("states: %s %s", state("a@example.com"), state("b@example.com"))
	}
	byEmail := []byte(`[{"event":"delivered","email":"b@example.com"},{"event":"email.bounced","recipient":"B@example.com"}]`)
	if code, res := e.webhook(t, "email", "X-Webhook-Signature", payments.Sign(emailSecret, byEmail), testAPIKey, byEmail); code != 200 || res["marked"] != float64(1) {
		t.Fatalf("bounce by email: %d %v", code, res)
	}
	if state("b@example.com") != "failed" {
		t.Fatalf("b not failed")
	}
	if code, _ := e.webhook(t, "smtp", "", "", testAPIKey, []byte(`{}`)); code != 404 {
		t.Fatalf("unknown provider: %d", code)
	}
}
