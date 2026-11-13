package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/authattempt"
	"github.com/intouch/api/internal/config"
	"github.com/intouch/api/internal/db"
	"github.com/intouch/api/internal/dbtest"
	"github.com/intouch/api/internal/gotrue"
)

// fakeAuth stands in for Supabase Auth: it registers users in the stub
// auth.users table, "sends" a fixed code and mints sessions signed with the
// test secret.
type fakeAuth struct {
	td    *dbtest.DB
	code  string
	fail  bool              // provider down: sends answer 500
	users map[string]string // email/phone → auth user id
	meta  map[string]string // user id → full_name
	sends []string          // identifiers a code was sent to
}

func newFake(td *dbtest.DB) *fakeAuth {
	return &fakeAuth{td: td, code: "482913", users: map[string]string{}, meta: map[string]string{}}
}

func (f *fakeAuth) addUser(t *testing.T, ident, name string) string {
	t.Helper()
	var id string
	if err := f.td.Pool.QueryRow(context.Background(),
		`insert into auth.users (email) values ($1) returning id::text`, ident).Scan(&id); err != nil {
		t.Fatal(err)
	}
	f.users[ident], f.meta[id] = id, name
	return id
}

func (f *fakeAuth) SignUp(ctx context.Context, email, _ string, md map[string]any) (*gotrue.User, error) {
	if f.fail {
		return nil, &gotrue.Error{Status: 500, Message: "Error sending confirmation email"}
	}
	if _, ok := f.users[email]; ok {
		return &gotrue.User{ID: "00000000-0000-0000-0000-000000000000", Identities: []any{}}, nil
	}
	var id string
	if err := f.td.Pool.QueryRow(ctx, `insert into auth.users (email) values ($1) returning id::text`, email).Scan(&id); err != nil {
		return nil, err
	}
	f.users[email], f.meta[id] = id, md["full_name"].(string)
	f.sends = append(f.sends, email)
	return &gotrue.User{ID: id, Email: email, Identities: []any{map[string]any{}}}, nil
}

func (f *fakeAuth) send(ident string) error {
	if f.fail {
		return &gotrue.Error{Status: 500, Message: "provider down"}
	}
	if _, ok := f.users[ident]; !ok {
		return &gotrue.Error{Status: 400, Code: "user_not_found"}
	}
	f.sends = append(f.sends, ident)
	return nil
}

func (f *fakeAuth) Resend(_ context.Context, e string) error  { return f.send(e) }
func (f *fakeAuth) Recover(_ context.Context, e string) error { return f.send(e) }
func (f *fakeAuth) SendSMS(_ context.Context, p string) error { return f.send(p) }

func (f *fakeAuth) Verify(_ context.Context, _, ident, code string) (*gotrue.Session, error) {
	id, ok := f.users[ident]
	if !ok || code != f.code {
		return nil, &gotrue.Error{Status: 403, Code: "otp_expired", Message: "Token has expired or is invalid"}
	}
	tok := auth.Sign(secret, map[string]any{"sub": id, "role": "authenticated", "aud": "authenticated",
		"exp": time.Now().Add(time.Hour).Unix(), "session_id": "11111111-1111-1111-1111-" + id[24:]})
	return &gotrue.Session{AccessToken: tok, RefreshToken: "r", ExpiresIn: 3600,
		User: gotrue.User{ID: id, UserMetadata: map[string]any{"full_name": f.meta[id]}}}, nil
}

type otpEnv struct {
	td   *dbtest.DB
	fake *fakeAuth
	r    *gin.Engine
}

func newOTPEnv(t *testing.T) *otpEnv {
	td := dbtest.New(t) // skips without TEST_DATABASE_URL
	gin.SetMode(gin.TestMode)
	f := newFake(td)
	r := New(Deps{Config: config.Config{JWTSecret: secret}, DB: &db.DB{Pool: td.Pool}, Auth: f})
	return &otpEnv{td: td, fake: f, r: r}
}

func (e *otpEnv) post(t *testing.T, path string, q url.Values, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	if q != nil {
		path += "?" + q.Encode()
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "test-device")
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

// age moves every logged attempt for ident back by d.
func (e *otpEnv) age(t *testing.T, ident string, d time.Duration) {
	t.Helper()
	n, _ := authattempt.NormalizeIdentifier(ident)
	if _, err := e.td.Pool.Exec(context.Background(),
		`update public.auth_attempts set created_at = created_at - $2::interval where identifier_hash = $1`,
		authattempt.Hash(n), d.String()); err != nil {
		t.Fatal(err)
	}
}

func (e *otpEnv) profileCount(t *testing.T, email string) int {
	t.Helper()
	var n int
	if err := e.td.Pool.QueryRow(context.Background(),
		`select count(*) from public.profiles p join auth.users u on u.id = p.id where u.email = $1`, email).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func msg(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

func signUpBody(email string) map[string]string {
	return map[string]string{"full_name": "Asha Rao", "email": email, "password": "Secret123", "confirm_password": "Secret123"}
}

func verifyQ(purpose, ident, code string) url.Values {
	return url.Values{"purpose": {purpose}, "identifier": {ident}, "code": {code}}
}

func TestSignUpValidation(t *testing.T) {
	e := newOTPEnv(t)
	long := strings.Repeat("a", 250) + "@x.com" // 256 chars
	cases := []struct {
		field, value, want string
	}{
		{"full_name", " A ", "Enter your name"},
		{"full_name", strings.Repeat("n", 81), "Enter your name"},
		{"email", long, "Enter a valid email"},
		{"email", "not-an-email", "Enter a valid email"},
		{"password", "Abc1234", "At least 8 characters"},
		{"password", strings.Repeat("A", 73), "Too long"},
		{"confirm_password", "Secret124", "Passwords do not match"},
	}
	for _, tc := range cases {
		b := signUpBody("asha@example.com")
		b[tc.field] = tc.value
		if tc.field == "password" {
			b["confirm_password"] = tc.value
		}
		w, out := e.post(t, "/auth/sign-up", nil, b)
		if w.Code != 400 || msg(out) != tc.want {
			t.Errorf("%s=%q: got %d %q, want 400 %q", tc.field, tc.value, w.Code, msg(out), tc.want)
		}
	}
	if len(e.fake.sends) != 0 {
		t.Fatalf("invalid sign-ups must not send: %v", e.fake.sends)
	}
}

func TestSignUpVerifyCreatesProfile(t *testing.T) {
	e := newOTPEnv(t)
	email := "asha@example.com"
	if w, out := e.post(t, "/auth/sign-up", nil, signUpBody(email)); w.Code != 202 || out["email"] != email {
		t.Fatalf("sign-up: %d %v", w.Code, out)
	}
	if e.profileCount(t, email) != 0 {
		t.Fatal("profile must not exist before verification")
	}
	// Resend inside the 30 s window (and at 20 s) is throttled.
	sq := url.Values{"purpose": {"signup"}, "identifier": {email}}
	if w, _ := e.post(t, "/auth/otp", sq, nil); w.Code != 429 {
		t.Fatalf("immediate resend: %d", w.Code)
	}
	e.age(t, email, 20*time.Second)
	if w, _ := e.post(t, "/auth/otp", sq, nil); w.Code != 429 {
		t.Fatalf("resend at 20 s: %d", w.Code)
	}
	e.age(t, email, 11*time.Second)
	if w, _ := e.post(t, "/auth/otp", sq, nil); w.Code != 202 {
		t.Fatalf("resend at 31 s: %d", w.Code)
	}

	// Five-digit code → 400 (not counted); wrong code / 123456 → 401.
	if w, out := e.post(t, "/auth/otp/verify", verifyQ("signup", email, "12345"), nil); w.Code != 400 || msg(out) != "Enter the full 6-digit code" {
		t.Fatalf("5 digits: %d %v", w.Code, out)
	}
	if w, out := e.post(t, "/auth/otp/verify", verifyQ("signup", email, "123456"), nil); w.Code != 401 || msg(out) != "Invalid code. Try again." {
		t.Fatalf("wrong code: %d %v", w.Code, out)
	}
	if e.profileCount(t, email) != 0 {
		t.Fatal("wrong code created a profile")
	}

	w, out := e.post(t, "/auth/otp/verify", verifyQ("signup", email, e.fake.code), nil)
	if w.Code != 200 {
		t.Fatalf("verify: %d %v", w.Code, out)
	}
	if out["onboarding_complete"] != false || out["full_name"] != "Asha Rao" {
		t.Fatalf("profile: %v", out)
	}
	if s, _ := out["session"].(map[string]any); s == nil || s["access_token"] == "" {
		t.Fatalf("no session: %v", out)
	}
	if e.profileCount(t, email) != 1 {
		t.Fatal("profile not created")
	}
	var devices int
	_ = e.td.Pool.QueryRow(context.Background(), `select count(*) from public.known_devices where user_id = $1`, out["id"]).Scan(&devices)
	if devices != 1 {
		t.Fatalf("known devices = %d", devices)
	}
	// The issued token is accepted by the API's verifier; a forged one is not.
	tok := out["session"].(map[string]any)["access_token"].(string)
	if _, err := auth.NewVerifier(secret).Verify(tok); err != nil {
		t.Fatalf("issued token rejected: %v", err)
	}
	if _, err := auth.NewVerifier(secret).Verify(auth.Sign("forged-secret-forged-secret-forged-1", map[string]any{"sub": out["id"]})); err == nil {
		t.Fatal("forged token accepted")
	}
}

func TestSignUpExistingEmailIsGeneric(t *testing.T) {
	e := newOTPEnv(t)
	e.fake.addUser(t, "taken@example.com", "Taken User")
	w, out := e.post(t, "/auth/sign-up", nil, signUpBody("taken@example.com"))
	if w.Code != 422 || strings.Contains(strings.ToLower(msg(out)), "exist") || strings.Contains(msg(out), "registered") {
		t.Fatalf("existing email: %d %q", w.Code, msg(out))
	}
}

func TestSignUpProviderFailureAllowsRetry(t *testing.T) {
	e := newOTPEnv(t)
	e.fake.fail = true
	if w, out := e.post(t, "/auth/sign-up", nil, signUpBody("asha@example.com")); w.Code != 502 || msg(out) == "" {
		t.Fatalf("provider down: %d %v", w.Code, out)
	}
	e.fake.fail = false
	if w, out := e.post(t, "/auth/sign-up", nil, signUpBody("asha@example.com")); w.Code != 202 {
		t.Fatalf("retry after failure must not be throttled: %d %v", w.Code, out)
	}
}

func TestVerifyExpiredAndAttemptCap(t *testing.T) {
	e := newOTPEnv(t)
	email := "ravi@example.com"
	if w, _ := e.post(t, "/auth/sign-up", nil, signUpBody(email)); w.Code != 202 {
		t.Fatal("sign-up")
	}
	for i := 1; i <= 5; i++ {
		if w, _ := e.post(t, "/auth/otp/verify", verifyQ("signup", email, "000000"), nil); w.Code != 401 {
			t.Fatalf("verify %d: %d", i, w.Code)
		}
	}
	// The 6th verify on this OTP is rejected, even with the right code.
	if w, _ := e.post(t, "/auth/otp/verify", verifyQ("signup", email, e.fake.code), nil); w.Code != 429 {
		t.Fatalf("6th verify: %d", w.Code)
	}

	email2 := "mira@example.com"
	if w, _ := e.post(t, "/auth/sign-up", nil, signUpBody(email2)); w.Code != 202 {
		t.Fatal("sign-up 2")
	}
	e.age(t, email2, 11*time.Minute)
	if w, _ := e.post(t, "/auth/otp/verify", verifyQ("signup", email2, e.fake.code), nil); w.Code != 410 {
		t.Fatalf("11-minute-old code: %d", w.Code)
	}
	if e.profileCount(t, email2) != 0 {
		t.Fatal("expired code created an account")
	}
}

func TestSendOTPIdenticalForUnknownAccount(t *testing.T) {
	e := newOTPEnv(t)
	e.fake.addUser(t, "known@example.com", "Known User")
	w1, _ := e.post(t, "/auth/otp", url.Values{"purpose": {"recovery"}, "identifier": {"known@example.com"}}, nil)
	w2, _ := e.post(t, "/auth/otp", url.Values{"purpose": {"recovery"}, "identifier": {"nobody@example.com"}}, nil)
	if w1.Code != 202 || w2.Code != 202 || w1.Body.String() != w2.Body.String() {
		t.Fatalf("recovery responses differ: %d %s / %d %s", w1.Code, w1.Body, w2.Code, w2.Body)
	}
}

func TestPhoneOTP(t *testing.T) {
	e := newOTPEnv(t)
	// Nine digits: 400 and no SMS.
	if w, _ := e.post(t, "/auth/otp", url.Values{"purpose": {"phone"}, "identifier": {"987654321"}}, nil); w.Code != 400 {
		t.Fatalf("9 digits: %d", w.Code)
	}
	if len(e.fake.sends) != 0 {
		t.Fatalf("SMS sent for invalid number: %v", e.fake.sends)
	}
	e.fake.addUser(t, "+919876543210", "Phone User")
	if w, _ := e.post(t, "/auth/otp", url.Values{"purpose": {"phone"}, "identifier": {"98765 43210"}}, nil); w.Code != 202 {
		t.Fatalf("phone send: %d", w.Code)
	}
	if w, _ := e.post(t, "/auth/otp/verify", verifyQ("phone", "9876543210", "111111"), nil); w.Code != 401 {
		t.Fatalf("wrong SMS code: %d", w.Code)
	}
	w, out := e.post(t, "/auth/otp/verify", verifyQ("phone", "9876543210", e.fake.code), nil)
	if w.Code != 200 || out["full_name"] != "Phone User" {
		t.Fatalf("phone verify: %d %v", w.Code, out)
	}
	if w, _ := e.post(t, "/auth/otp", url.Values{"purpose": {"bogus"}, "identifier": {"a@b.co"}}, nil); w.Code != 400 {
		t.Fatalf("bad purpose: %d", w.Code)
	}
}
