package config

import (
	"strings"
	"testing"
)

func TestValidateAndRedaction(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	c := FromLookup(get)
	if c.Port != "8080" {
		t.Fatalf("default port %q", c.Port)
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL, SUPABASE_JWT_SECRET, SUPABASE_URL, WEBHOOK_API_KEY") {
		t.Fatalf("validate: %v", err)
	}
	env = map[string]string{
		"DATABASE_URL": "postgres://u:dbpass@h/db", "SUPABASE_URL": "https://x.supabase.co/",
		"SUPABASE_JWT_SECRET": strings.Repeat("s", 40), "WEBHOOK_API_KEY": "hookkey",
		"RAZORPAY_KEY_ID": "rzp", "RAZORPAY_KEY_SECRET": "rzpsecret",
	}
	c = FromLookup(get)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.SupabaseURL != "https://x.supabase.co" || !c.PaymentsEnabled() {
		t.Fatalf("%+v", c)
	}
	s := c.String()
	for _, secret := range []string{"dbpass", "sssss", "hookkey", "rzpsecret"} {
		if strings.Contains(s, secret) {
			t.Fatalf("String leaks %q: %s", secret, s)
		}
	}
}
