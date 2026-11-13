// Package config loads the service settings and provider secrets from the
// environment. Secrets never appear in String() or logs.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config is the runtime configuration of the API.
type Config struct {
	Port string // PORT, default 8080

	DatabaseURL string // DATABASE_URL: pooled Postgres URL of the API login role

	SupabaseURL        string // SUPABASE_URL
	SupabaseAnonKey    string // SUPABASE_ANON_KEY
	SupabaseServiceKey string // SUPABASE_SERVICE_ROLE_KEY (secret)
	JWTSecret          string // SUPABASE_JWT_SECRET (secret): HS256 key that signs Supabase access tokens

	WebhookAPIKey string // WEBHOOK_API_KEY (secret): X-API-Key for /internal/*

	// Third-party providers; empty means "not configured" and the endpoints
	// that need them answer 503 (or skip the side effect).
	EmailAPIKey       string // EMAIL_API_KEY
	EmailFrom         string // EMAIL_FROM
	EmailWebhook      string // EMAIL_WEBHOOK_SECRET: HMAC key of the bounce webhook
	PushAPIKey        string // PUSH_API_KEY
	GIFAPIKey         string // GIF_API_KEY
	RazorpayKeyID     string // RAZORPAY_KEY_ID
	RazorpayKeySecret string // RAZORPAY_KEY_SECRET
	RazorpayWebhook   string // RAZORPAY_WEBHOOK_SECRET

	// AppLinkBase prefixes links in emails, e.g. invite links
	// <base>communities/<id>/invite/<code>. APP_LINK_BASE, default "intouch://".
	AppLinkBase string
}

// Load reads the configuration from the environment.
func Load() Config {
	return FromLookup(os.Getenv)
}

// FromLookup reads the configuration through get (os.Getenv in production).
func FromLookup(get func(string) string) Config {
	c := Config{
		Port:               get("PORT"),
		DatabaseURL:        get("DATABASE_URL"),
		SupabaseURL:        strings.TrimRight(get("SUPABASE_URL"), "/"),
		SupabaseAnonKey:    get("SUPABASE_ANON_KEY"),
		SupabaseServiceKey: get("SUPABASE_SERVICE_ROLE_KEY"),
		JWTSecret:          get("SUPABASE_JWT_SECRET"),
		WebhookAPIKey:      get("WEBHOOK_API_KEY"),
		EmailAPIKey:        get("EMAIL_API_KEY"),
		EmailFrom:          get("EMAIL_FROM"),
		EmailWebhook:       get("EMAIL_WEBHOOK_SECRET"),
		PushAPIKey:         get("PUSH_API_KEY"),
		GIFAPIKey:          get("GIF_API_KEY"),
		RazorpayKeyID:      get("RAZORPAY_KEY_ID"),
		RazorpayKeySecret:  get("RAZORPAY_KEY_SECRET"),
		RazorpayWebhook:    get("RAZORPAY_WEBHOOK_SECRET"),
		AppLinkBase:        get("APP_LINK_BASE"),
	}
	if c.AppLinkBase == "" {
		c.AppLinkBase = "intouch://"
	}
	if !strings.HasSuffix(c.AppLinkBase, "/") {
		c.AppLinkBase += "/"
	}
	if c.Port == "" {
		c.Port = "8080"
	}
	return c
}

// Validate reports the settings a deployed service cannot run without.
// Provider keys are optional.
func (c Config) Validate() error {
	var missing []string
	for name, v := range map[string]string{
		"DATABASE_URL":        c.DatabaseURL,
		"SUPABASE_URL":        c.SupabaseURL,
		"SUPABASE_JWT_SECRET": c.JWTSecret,
		"WEBHOOK_API_KEY":     c.WebhookAPIKey,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required settings: %s", strings.Join(sorted(missing), ", "))
	}
	if len(c.JWTSecret) < 32 {
		return fmt.Errorf("SUPABASE_JWT_SECRET must be at least 32 characters")
	}
	return nil
}

// PaymentsEnabled reports whether the Razorpay gateway is configured.
func (c Config) PaymentsEnabled() bool {
	return c.RazorpayKeyID != "" && c.RazorpayKeySecret != ""
}

// String describes the configuration without revealing any secret.
func (c Config) String() string {
	set := func(s string) string {
		if s == "" {
			return "unset"
		}
		return "set"
	}
	return fmt.Sprintf("port=%s supabase_url=%s database=%s jwt_secret=%s webhook_key=%s email=%s push=%s gif=%s razorpay=%s",
		c.Port, c.SupabaseURL, set(c.DatabaseURL), set(c.JWTSecret), set(c.WebhookAPIKey),
		set(c.EmailAPIKey), set(c.PushAPIKey), set(c.GIFAPIKey), set(c.RazorpayKeyID))
}

func sorted(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}
