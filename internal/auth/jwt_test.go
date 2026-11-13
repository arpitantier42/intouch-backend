package auth

import (
	"errors"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	const s = "unit-secret-unit-secret-unit-secret-12"
	v := NewVerifier(s)
	now := time.Unix(1_800_000_000, 0)
	v.now = func() time.Time { return now }
	ok := map[string]any{"sub": "u1", "role": "authenticated", "aud": "authenticated", "exp": now.Add(time.Minute).Unix(), "session_id": "s1"}

	c, err := v.Verify(Sign(s, ok))
	if err != nil || c.Subject != "u1" || c.SessionID != "s1" || len(c.Raw) == 0 {
		t.Fatalf("valid: %v %+v", err, c)
	}
	arr := map[string]any{"sub": "u1", "role": "authenticated", "aud": []string{"x", "authenticated"}, "exp": now.Add(time.Minute).Unix()}
	if _, err := v.Verify(Sign(s, arr)); err != nil {
		t.Fatalf("aud array: %v", err)
	}
	expired := map[string]any{"sub": "u1", "role": "authenticated", "aud": "authenticated", "exp": now.Unix()}
	if _, err := v.Verify(Sign(s, expired)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := v.Verify(Sign("other", ok)); !errors.Is(err, ErrSignature) {
		t.Fatalf("bad sig: %v", err)
	}
	svc := map[string]any{"role": "service_role", "exp": now.Add(time.Minute).Unix()}
	if _, err := v.Verify(Sign(s, svc)); !errors.Is(err, ErrClaims) {
		t.Fatalf("service token as user: %v", err)
	}
	// alg=none must be refused.
	tok := Sign(s, ok)
	none := "eyJhbGciOiJub25lIn0" + tok[len("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"):]
	if _, err := v.Verify(none); !errors.Is(err, ErrMalformed) {
		t.Fatalf("alg none: %v", err)
	}
	if _, err := NewVerifier("").Verify(tok); err == nil {
		t.Fatal("empty secret must reject")
	}
	if KeyEqual("", "") || !KeyEqual("k", "k") || KeyEqual("k", "j") {
		t.Fatal("KeyEqual")
	}
}
