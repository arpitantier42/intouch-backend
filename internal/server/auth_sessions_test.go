package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/authattempt"
	"github.com/intouch/api/internal/config"
	"github.com/intouch/api/internal/db"
	"github.com/intouch/api/internal/dbtest"
	"github.com/intouch/api/internal/gotrue"
	"github.com/intouch/api/internal/mailer"
)

// --- fake Auth: password grant, update user, logout ---

func (f *fakeAuth) token(id, session string, amr string) string {
	claims := map[string]any{"sub": id, "role": "authenticated", "aud": "authenticated",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "session_id": session}
	for e, uid := range f.users {
		if uid == id && strings.Contains(e, "@") {
			claims["email"] = e
		}
	}
	if amr != "" {
		claims["amr"] = []map[string]any{{"method": amr, "timestamp": time.Now().Unix()}}
	}
	return auth.Sign(secret, claims)
}

func (f *fakeAuth) SignInPassword(_ context.Context, email, password string) (*gotrue.Session, error) {
	if f.fail {
		return nil, &gotrue.Error{Status: 500, Message: "down"}
	}
	id, ok := f.users[email]
	if !ok || f.passwords[email] != password {
		return nil, &gotrue.Error{Status: 400, Code: "invalid_credentials", Message: "Invalid login credentials"}
	}
	f.nsess++
	sid := fmt.Sprintf("22222222-2222-2222-2222-%012d", f.nsess)
	return &gotrue.Session{AccessToken: f.token(id, sid, "password"), RefreshToken: "r", ExpiresIn: 3600,
		User: gotrue.User{ID: id, Email: email, UserMetadata: map[string]any{"full_name": f.meta[id]}}}, nil
}

func (f *fakeAuth) UpdatePassword(_ context.Context, tok, password string) error {
	c, err := auth.NewVerifier(secret).Verify(tok)
	if err != nil {
		return &gotrue.Error{Status: 401}
	}
	for e, id := range f.users {
		if id == c.Subject {
			f.passwords[e] = password
		}
	}
	return nil
}

func (f *fakeAuth) Logout(_ context.Context, _, scope string) error {
	f.logouts = append(f.logouts, scope)
	return nil
}

// fakeMailer records sent mail.
type fakeMailer struct {
	mu   sync.Mutex
	sent []mailer.Message
}

func (m *fakeMailer) Send(_ context.Context, msg mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

func (m *fakeMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

// --- environment ---

type sessEnv struct {
	*otpEnv
	mail *fakeMailer
}

func newSessEnv(t *testing.T) *sessEnv {
	td := dbtest.New(t)
	gin.SetMode(gin.TestMode)
	f := newFake(td)
	m := &fakeMailer{}
	r := New(Deps{Config: config.Config{JWTSecret: secret}, DB: &db.DB{Pool: td.Pool}, Auth: f, Mailer: m})
	return &sessEnv{otpEnv: &otpEnv{td: td, fake: f, r: r}, mail: m}
}

// user registers an Auth user with a password.
func (e *sessEnv) user(t *testing.T, email, password string) string {
	id := e.fake.addUser(t, email, "Asha Rao")
	e.fake.passwords[email] = password
	return id
}

func (e *sessEnv) do(t *testing.T, method, path, token, device string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "test-agent")
	if device != "" {
		req.Header.Set("X-Device-Id", device)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (e *sessEnv) signIn(t *testing.T, email, password, device string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return e.do(t, http.MethodPost, "/auth/sign-in?email="+url.QueryEscape(email), "", device,
		map[string]string{"password": password})
}

// session signs in and returns the access token.
func (e *sessEnv) session(t *testing.T, email, password, device string) string {
	t.Helper()
	w, out := e.signIn(t, email, password, device)
	if w.Code != 200 {
		t.Fatalf("sign-in: %d %v", w.Code, out)
	}
	return out["session"].(map[string]any)["access_token"].(string)
}

func (e *sessEnv) failures(t *testing.T, email string) int {
	t.Helper()
	var n int
	if err := e.td.Pool.QueryRow(context.Background(), `select public.auth_password_failures($1)`,
		authattempt.Hash(email)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *sessEnv) devices(t *testing.T, token string) []any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/me/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("devices: %d %s", w.Code, w.Body)
	}
	var out []any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func (e *sessEnv) status(t *testing.T, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/me/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w.Code
}

// --- tests ---

func TestSignInWrongCredentialsAreGeneric(t *testing.T) {
	e := newSessEnv(t)
	e.user(t, "asha@example.com", "Secret123")
	w1, _ := e.signIn(t, "asha@example.com", "Wrong1234", "")
	w2, _ := e.signIn(t, "nobody@example.com", "Wrong1234", "")
	if w1.Code != 401 || w2.Code != 401 || w1.Body.String() != w2.Body.String() {
		t.Fatalf("wrong password vs unknown email differ: %d %s / %d %s", w1.Code, w1.Body, w2.Code, w2.Body)
	}
	if w, _ := e.signIn(t, "", "x", ""); w.Code != 400 {
		t.Fatalf("missing email: %d", w.Code)
	}
}

func TestSignInLockoutAndReset(t *testing.T) {
	e := newSessEnv(t)
	email := "ravi@example.com"
	e.user(t, email, "Secret123")

	// Three failures, then a success: the failure count resets to zero.
	for i := 0; i < 3; i++ {
		e.signIn(t, email, "Wrong1234", "")
	}
	if n := e.failures(t, email); n != 3 {
		t.Fatalf("failures = %d", n)
	}
	e.session(t, email, "Secret123", "d1")
	if n := e.failures(t, email); n != 0 {
		t.Fatalf("failures after success = %d", n)
	}

	// Ten consecutive failures lock the account, even for the right password.
	for i := 1; i <= 10; i++ {
		if w, _ := e.signIn(t, email, "Wrong1234", ""); w.Code != 401 {
			t.Fatalf("failure %d: %d", i, w.Code)
		}
	}
	if w, out := e.signIn(t, email, "Secret123", ""); w.Code != 423 || msg(out) == "" {
		t.Fatalf("locked: %d %v", w.Code, out)
	}
	e.age(t, email, 14*time.Minute)
	if w, _ := e.signIn(t, email, "Secret123", ""); w.Code != 423 {
		t.Fatalf("still locked at 14 min: %d", w.Code)
	}
	e.age(t, email, 2*time.Minute)
	if w, out := e.signIn(t, email, "Secret123", ""); w.Code != 200 {
		t.Fatalf("after 16 min: %d %v", w.Code, out)
	}
	if n := e.failures(t, email); n != 0 {
		t.Fatalf("failures after unlock = %d", n)
	}
}

func TestSignInNewDeviceAlert(t *testing.T) {
	e := newSessEnv(t)
	id := e.user(t, "mira@example.com", "Secret123")
	w, out := e.signIn(t, "mira@example.com", "Secret123", "phone-a")
	if w.Code != 200 || out["id"] != id || out["session"] == nil {
		t.Fatalf("sign-in: %d %v", w.Code, out)
	}
	e.session(t, "mira@example.com", "Secret123", "phone-a") // same device: no alert
	e.session(t, "mira@example.com", "Secret123", "laptop-b")
	var alerts int
	_ = e.td.Pool.QueryRow(context.Background(),
		`select count(*) from public.notifications where user_id = $1 and kind = 'login_alert'`, id).Scan(&alerts)
	if alerts != 2 {
		t.Fatalf("login_alert notifications = %d, want 2 (two distinct devices)", alerts)
	}
	deadline := time.Now().Add(2 * time.Second)
	for e.mail.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := e.mail.count(); n != 2 || e.mail.sent[0].To != "mira@example.com" {
		t.Fatalf("alert emails = %d %v", n, e.mail.sent)
	}

	// Alerts off: a new device sends nothing.
	if _, err := e.td.Pool.Exec(context.Background(), `update public.profiles set login_alerts = false where id = $1`, id); err != nil {
		t.Fatal(err)
	}
	e.session(t, "mira@example.com", "Secret123", "tablet-c")
	time.Sleep(50 * time.Millisecond)
	if n := e.mail.count(); n != 2 {
		t.Fatalf("alert sent with alerts off: %d", n)
	}
}

func TestSignInTwoFactorRequiresSMS(t *testing.T) {
	e := newSessEnv(t)
	id := e.user(t, "dev@example.com", "Secret123")
	e.session(t, "dev@example.com", "Secret123", "") // creates the profile
	if _, err := e.td.Pool.Exec(context.Background(),
		`update public.profiles set phone = '+919876543210', two_factor_enabled = true where id = $1`, id); err != nil {
		t.Fatal(err)
	}
	e.fake.users["+919876543210"] = id
	e.fake.sends = nil

	w, out := e.signIn(t, "dev@example.com", "Secret123", "")
	if w.Code != 202 || out["session"] != nil || out["two_factor_required"] != true || out["identifier"] != "+919876543210" {
		t.Fatalf("2FA sign-in: %d %v", w.Code, out)
	}
	if len(e.fake.sends) != 1 || e.fake.sends[0] != "+919876543210" {
		t.Fatalf("SMS sends: %v", e.fake.sends)
	}
	// A wrong password never reaches the SMS step.
	if w, _ := e.signIn(t, "dev@example.com", "Wrong1234", ""); w.Code != 401 {
		t.Fatalf("2FA wrong password: %d", w.Code)
	}
	w, out = e.post(t, "/auth/otp/verify", verifyQ("two_factor", "+919876543210", e.fake.code), nil)
	if w.Code != 200 || out["session"] == nil {
		t.Fatalf("2FA verify: %d %v", w.Code, out)
	}
}

func TestSignOutLocalRejectsOldToken(t *testing.T) {
	e := newSessEnv(t)
	e.user(t, "lee@example.com", "Secret123")
	tok := e.session(t, "lee@example.com", "Secret123", "d1")
	if d := e.devices(t, tok); len(d) != 1 || d[0].(map[string]any)["current"] != true {
		t.Fatalf("devices: %v", d)
	}
	if w, _ := e.do(t, http.MethodPost, "/auth/sign-out", tok, "", nil); w.Code != 400 {
		t.Fatalf("missing scope: %d", w.Code)
	}
	if w, out := e.do(t, http.MethodPost, "/auth/sign-out?scope=local", tok, "", nil); w.Code != 204 {
		t.Fatalf("sign-out: %d %v", w.Code, out)
	}
	if s := e.status(t, tok); s != 401 {
		t.Fatalf("old token after sign-out: %d", s)
	}
	if len(e.fake.logouts) != 1 || e.fake.logouts[0] != "local" {
		t.Fatalf("auth logouts: %v", e.fake.logouts)
	}
	tok2 := e.session(t, "lee@example.com", "Secret123", "d1")
	if d := e.devices(t, tok2); len(d) != 1 {
		t.Fatalf("devices after re-sign-in: %v", d)
	}
}

func TestSignOutOthers(t *testing.T) {
	e := newSessEnv(t)
	e.user(t, "kim@example.com", "Secret123")
	a := e.session(t, "kim@example.com", "Secret123", "phone")
	b := e.session(t, "kim@example.com", "Secret123", "laptop")
	if d := e.devices(t, b); len(d) != 2 {
		t.Fatalf("devices: %v", d)
	}
	if w, _ := e.do(t, http.MethodPost, "/auth/sign-out?scope=others", b, "", nil); w.Code != 204 {
		t.Fatalf("sign-out others: %d", w.Code)
	}
	if s := e.status(t, a); s != 401 {
		t.Fatalf("other session still valid: %d", s)
	}
	if d := e.devices(t, b); len(d) != 1 {
		t.Fatalf("devices after sign-out others: %v", d)
	}
	time.Sleep(1100 * time.Millisecond) // next token's iat is after the cutoff
	if c := e.session(t, "kim@example.com", "Secret123", "phone"); e.status(t, c) != 200 {
		t.Fatal("new sign-in after sign-out others rejected")
	}
}

func TestSetPassword(t *testing.T) {
	e := newSessEnv(t)
	e.user(t, "ana@example.com", "Secret123")
	other := e.session(t, "ana@example.com", "Secret123", "phone")
	me := e.session(t, "ana@example.com", "Secret123", "laptop")
	post := func(tok string, body map[string]string) (*httptest.ResponseRecorder, map[string]any) {
		return e.do(t, http.MethodPost, "/auth/password", tok, "", body)
	}
	if w, out := post(me, map[string]string{"current_password": "Secret123", "new_password": "password1"}); w.Code != 400 || msg(out) != "Use upper and lower case letters and a digit" {
		t.Fatalf("password1: %d %v", w.Code, out)
	}
	if w, _ := post(me, map[string]string{"current_password": "Secret123", "new_password": "Ab1"}); w.Code != 400 {
		t.Fatalf("short: %d", w.Code)
	}
	if w, _ := post(me, map[string]string{"current_password": "Nope12345", "new_password": "Password1"}); w.Code != 401 {
		t.Fatalf("wrong current: %d", w.Code)
	}
	if w, _ := post(me, map[string]string{"new_password": "Password1"}); w.Code != 401 {
		t.Fatalf("no current: %d", w.Code)
	}
	if e.fake.passwords["ana@example.com"] != "Secret123" {
		t.Fatal("password changed by a rejected request")
	}
	// Meets the policy without a symbol: accepted.
	if w, out := post(me, map[string]string{"current_password": "Secret123", "new_password": "Password1"}); w.Code != 204 {
		t.Fatalf("change: %d %v", w.Code, out)
	}
	if e.fake.passwords["ana@example.com"] != "Password1" {
		t.Fatal("password not updated")
	}
	if e.status(t, other) != 401 {
		t.Fatal("other session survived the password change")
	}
	if e.status(t, me) != 200 {
		t.Fatal("current session signed out by its own password change")
	}

	// Recovery session: no current password needed.
	id := e.fake.users["ana@example.com"]
	time.Sleep(1100 * time.Millisecond)
	rec := e.fake.token(id, "33333333-3333-3333-3333-333333333333", "recovery")
	if w, out := post(rec, map[string]string{"new_password": "Recovered9"}); w.Code != 204 {
		t.Fatalf("recovery reset: %d %v", w.Code, out)
	}
	if e.status(t, me) != 401 {
		t.Fatal("reset did not sign out other sessions")
	}
	if w, _ := e.signIn(t, "ana@example.com", "Recovered9", ""); w.Code != 200 {
		t.Fatalf("sign-in with new password: %d", w.Code)
	}
}
