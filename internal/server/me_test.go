package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/intouch/api/internal/auth"
)

// googleToken mints an access token like Supabase issues after Google OAuth.
func googleToken(id string, exp time.Time) string {
	return auth.Sign(secret, map[string]any{"sub": id, "role": "authenticated", "aud": "authenticated",
		"iat": time.Now().Unix(), "exp": exp.Unix(), "session_id": "11111111-1111-1111-1111-111111111111",
		"app_metadata":  map[string]any{"provider": "google"},
		"user_metadata": map[string]any{"full_name": "Meera Iyer", "avatar_url": "https://lh3.example.com/a.png"}})
}

// meUser creates an Auth user (as Google would) and returns its token.
func meUser(t *testing.T, e *sessEnv, email string) (string, string) {
	t.Helper()
	id := e.fake.addUser(t, email, "")
	return id, googleToken(id, time.Now().Add(time.Hour))
}

func TestGetMeCreatesProfileOnFirstGoogleSignIn(t *testing.T) {
	e := newSessEnv(t)
	id, tok := meUser(t, e, "meera@example.com")

	if w, _ := e.do(t, http.MethodGet, "/me", "", "", nil); w.Code != 401 {
		t.Fatalf("no session: %d", w.Code)
	}
	if w, _ := e.do(t, http.MethodGet, "/me", googleToken(id, time.Now().Add(-time.Minute)), "", nil); w.Code != 401 {
		t.Fatalf("expired session: %d", w.Code)
	}
	if n := e.profileCount(t, "meera@example.com"); n != 0 {
		t.Fatalf("profile before first /me: %d", n)
	}

	w, out := e.do(t, http.MethodGet, "/me", tok, "", nil)
	if w.Code != 200 {
		t.Fatalf("getMe: %d %v", w.Code, out)
	}
	if out["id"] != id || out["full_name"] != "Meera Iyer" || out["onboarding_complete"] != false ||
		out["avatar_url"] != "https://lh3.example.com/a.png" || out["deletion_requested_at"] != nil {
		t.Fatalf("new profile: %v", out)
	}

	// Returning user: onboarded profile is returned unchanged.
	if w, out := e.do(t, http.MethodPatch, "/me", tok, "", map[string]any{"username": "meera", "onboarding_complete": true}); w.Code != 200 {
		t.Fatalf("patch: %d %v", w.Code, out)
	}
	_, out = e.do(t, http.MethodGet, "/me", tok, "", nil)
	if out["onboarding_complete"] != true || out["username"] != "meera" {
		t.Fatalf("returning: %v", out)
	}
	if n := e.profileCount(t, "meera@example.com"); n != 1 {
		t.Fatalf("profiles: %d", n)
	}
}

func TestUpdateMeValidation(t *testing.T) {
	e := newSessEnv(t)
	_, tok := meUser(t, e, "v@example.com")
	cases := []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"username": "has space"}, "username"},
		{map[string]any{"username": "ab"}, "username"},
		{map[string]any{"username": "Upper"}, "username"},
		{map[string]any{"bio": strings.Repeat("x", 200)}, "bio"},
		{map[string]any{"full_name": "A"}, "full_name"},
		{map[string]any{"headline": strings.Repeat("h", 121)}, "headline"},
		{map[string]any{"skills": []any{"go", 3}}, "skills"},
		{map[string]any{"profile_visibility": "everyone"}, "profile_visibility"},
		{map[string]any{"hide_city": "yes"}, "hide_city"},
	}
	for _, tc := range cases {
		w, out := e.do(t, http.MethodPatch, "/me", tok, "", tc.body)
		fields, _ := out["error"].(map[string]any)["fields"].(map[string]any)
		if w.Code != 400 || fields[tc.field] == nil || msg(out) == "" {
			t.Errorf("%v: %d %v", tc.body, w.Code, out)
		}
	}
	if w, _ := e.do(t, http.MethodPatch, "/me", tok, "", nil); w.Code != 400 {
		t.Errorf("no body: %d", w.Code)
	}
	_, out := e.do(t, http.MethodGet, "/me", tok, "", nil)
	if out["username"] != nil {
		t.Fatalf("invalid patch saved: %v", out)
	}
}

func TestUpdateMeOnboardingChipsAndSkip(t *testing.T) {
	e := newSessEnv(t)
	_, tok := meUser(t, e, "o@example.com")

	// Skip: profile fields saved, onboarding_complete stays false.
	w, out := e.do(t, http.MethodPatch, "/me", tok, "", map[string]any{
		"full_name": "  Meera I  ", "headline": "Engineer", "company": "", "city": "Pune",
		"bio": strings.Repeat("b", 160), "is_operator": true, "deletion_requested_at": "2020-01-01T00:00:00Z",
		"skills": []string{"Go", "go ", "SQL", "", "Go"}, "interests": []string{"Hiking", "hiking"},
	})
	if w.Code != 200 {
		t.Fatalf("patch: %d %v", w.Code, out)
	}
	if out["full_name"] != "Meera I" || out["company"] != nil || out["city"] != "Pune" ||
		out["onboarding_complete"] != false || out["is_operator"] != false || out["deletion_requested_at"] != nil {
		t.Fatalf("saved: %v", out)
	}
	if s := out["skills"].([]any); len(s) != 2 || s[0] != "Go" || s[1] != "SQL" {
		t.Fatalf("skills: %v", s)
	}
	if s := out["interests"].([]any); len(s) != 1 {
		t.Fatalf("interests: %v", s)
	}

	// No chips picked: onboarding still completes.
	w, out = e.do(t, http.MethodPatch, "/me", tok, "", map[string]any{
		"skills": []string{}, "interests": nil, "onboarding_complete": true,
		"profile_visibility": "connections", "login_alerts": false})
	if w.Code != 200 || out["onboarding_complete"] != true || len(out["skills"].([]any)) != 0 ||
		out["profile_visibility"] != "connections" || out["login_alerts"] != false {
		t.Fatalf("complete: %d %v", w.Code, out)
	}
}

func TestUpdateMeUsernameTaken(t *testing.T) {
	e := newSessEnv(t)
	_, a := meUser(t, e, "a@example.com")
	_, b := meUser(t, e, "b@example.com")
	if w, out := e.do(t, http.MethodPatch, "/me", a, "", map[string]any{"username": "asha"}); w.Code != 200 {
		t.Fatalf("a: %d %v", w.Code, out)
	}
	w, out := e.do(t, http.MethodPatch, "/me", b, "", map[string]any{"username": "asha"})
	if w.Code != 409 || msg(out) != msgUsernameTaken {
		t.Fatalf("taken: %d %v", w.Code, out)
	}
	// Saving one's own username again is fine.
	if w, _ := e.do(t, http.MethodPatch, "/me", a, "", map[string]any{"username": "asha"}); w.Code != 200 {
		t.Fatalf("same user: %d", w.Code)
	}
}

func TestUpdateMeConcurrentUsername(t *testing.T) {
	e := newSessEnv(t)
	toks := make([]string, 6)
	for i := range toks {
		_, toks[i] = meUser(t, e, string(rune('a'+i))+"c@example.com")
		e.do(t, http.MethodGet, "/me", toks[i], "", nil)
	}
	codes := make([]int, len(toks))
	var wg sync.WaitGroup
	for i, tok := range toks {
		wg.Add(1)
		go func(i int, tok string) {
			defer wg.Done()
			w, _ := e.do(t, http.MethodPatch, "/me", tok, "", map[string]any{"username": "same.name"})
			codes[i] = w.Code
		}(i, tok)
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 409:
			conflict++
		}
	}
	if ok != 1 || conflict != len(toks)-1 {
		t.Fatalf("codes: %v", codes)
	}
}

func TestUpdateMeTwoFactorNeedsVerifiedPhone(t *testing.T) {
	e := newSessEnv(t)
	id, tok := meUser(t, e, "p@example.com")
	w, out := e.do(t, http.MethodPatch, "/me", tok, "", map[string]any{"two_factor_enabled": true, "phone": "+919876543210"})
	if w.Code != 400 || !strings.Contains(msg(out), "phone") {
		t.Fatalf("no phone: %d %v", w.Code, out)
	}
	ctx := context.Background()
	// An unconfirmed phone is not enough.
	if _, err := e.td.Pool.Exec(ctx, `update auth.users set phone = '919876543210' where id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if w, _ := e.do(t, http.MethodPatch, "/me", tok, "", map[string]any{"two_factor_enabled": true}); w.Code != 400 {
		t.Fatalf("unconfirmed: %d", w.Code)
	}
	if _, err := e.td.Pool.Exec(ctx, `update auth.users set phone_confirmed_at = now() where id = $1`, id); err != nil {
		t.Fatal(err)
	}
	w, out = e.do(t, http.MethodPatch, "/me", tok, "", map[string]any{"two_factor_enabled": true})
	if w.Code != 200 || out["two_factor_enabled"] != true || out["phone"] != "+919876543210" {
		t.Fatalf("enable: %d %v", w.Code, out)
	}
	w, out = e.do(t, http.MethodPatch, "/me", tok, "", map[string]any{"two_factor_enabled": false})
	if w.Code != 200 || out["two_factor_enabled"] != false {
		t.Fatalf("disable: %d %v", w.Code, out)
	}
}

func TestAccountDeletion(t *testing.T) {
	e := newSessEnv(t)
	id, tok := meUser(t, e, "d@example.com")

	if w, out := e.do(t, http.MethodDelete, "/me/deletion", tok, "", nil); w.Code != 404 {
		t.Fatalf("cancel none: %d %v", w.Code, out)
	}
	for _, q := range []string{"?reason=moving&confirm=delete", "?reason=moving", "?confirm=DELETE", "?reason=%20&confirm=DELETE"} {
		if w, out := e.do(t, http.MethodPost, "/me/deletion"+q, tok, "", nil); w.Code != 400 {
			t.Fatalf("%s: %d %v", q, w.Code, out)
		}
	}
	w, out := e.do(t, http.MethodPost, "/me/deletion?reason=Too+many+apps&confirm=DELETE", tok, "", nil)
	if w.Code != 200 || out["deletion_requested_at"] == nil || out["deletion_reason"] != "Too many apps" {
		t.Fatalf("request: %d %v", w.Code, out)
	}
	// Signing in during the grace period: /me carries the pending state.
	if _, out := e.do(t, http.MethodGet, "/me", tok, "", nil); out["deletion_requested_at"] == nil {
		t.Fatalf("me: %v", out)
	}
	// JSON body form is accepted too.
	if w, out := e.do(t, http.MethodPost, "/me/deletion", tok, "", map[string]string{"reason": "x", "confirm": "DELETE"}); w.Code != 200 {
		t.Fatalf("body form: %d %v", w.Code, out)
	}

	ctx := context.Background()
	due := func() int {
		var n int
		if err := e.td.Pool.QueryRow(ctx, `select count(*) from public.accounts_due_for_purge(10) where user_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if due() != 0 {
		t.Fatal("due before 14 days")
	}
	if _, err := e.td.Pool.Exec(ctx, `update public.profiles set deletion_requested_at = now() - interval '14 days 1 minute' where id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if due() != 1 {
		t.Fatal("not due after 14 days")
	}

	w, out = e.do(t, http.MethodDelete, "/me/deletion", tok, "", nil)
	if w.Code != 200 || out["deletion_requested_at"] != nil || out["deletion_reason"] != nil {
		t.Fatalf("cancel: %d %v", w.Code, out)
	}
	if due() != 0 {
		t.Fatal("due after cancel")
	}
}
