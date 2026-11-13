// Package gotrue is a minimal client for the Supabase Auth (GoTrue) REST API
// used by the auth wrappers. Handlers depend on the Client interface so tests
// can substitute a fake.
package gotrue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is what the auth endpoints need from Supabase Auth.
type Client interface {
	// SignUp registers an unconfirmed email user; Auth emails a 6-digit OTP
	// (the "Confirm signup" template must use {{ .Token }}).
	SignUp(ctx context.Context, email, password string, metadata map[string]any) (*User, error)
	// Resend re-sends the sign-up confirmation OTP.
	Resend(ctx context.Context, email string) error
	// Recover sends the password-recovery OTP.
	Recover(ctx context.Context, email string) error
	// SendSMS sends an SMS OTP to an existing phone user (never creates one).
	SendSMS(ctx context.Context, phone string) error
	// Verify checks a code. typ is signup|recovery|sms; identifier is the
	// email (signup, recovery) or E.164 phone (sms).
	Verify(ctx context.Context, typ, identifier, code string) (*Session, error)
}

// User is the part of an Auth user the API reads.
type User struct {
	ID           string         `json:"id"`
	Email        string         `json:"email"`
	Phone        string         `json:"phone"`
	UserMetadata map[string]any `json:"user_metadata"`
	Identities   []any          `json:"identities"`
}

// Session is an issued Auth session.
type Session struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	User         User   `json:"user"`
}

// Error is a non-2xx answer from Auth (Status 0 = transport failure).
type Error struct {
	Status  int
	Code    string // error_code, e.g. otp_expired, user_already_exists
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("supabase auth: %d %s %s", e.Status, e.Code, e.Message)
}

// StatusOf returns the Auth HTTP status of err (0 for transport errors).
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// HTTP talks to a Supabase project's Auth endpoint (<SUPABASE_URL>/auth/v1).
type HTTP struct {
	BaseURL string // SUPABASE_URL
	APIKey  string // anon key
	Client  *http.Client
}

// New returns an HTTP client with a 10 s timeout.
func New(baseURL, anonKey string) *HTTP {
	return &HTTP{BaseURL: baseURL, APIKey: anonKey, Client: &http.Client{Timeout: 10 * time.Second}}
}

func (h *HTTP) post(ctx context.Context, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.BaseURL+"/auth/v1"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", h.APIKey)
	req.Header.Set("Authorization", "Bearer "+h.APIKey)
	res, err := h.Client.Do(req)
	if err != nil {
		return &Error{Status: 0, Message: err.Error()}
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Code      any    `json:"code"`
			ErrorCode string `json:"error_code"`
			Msg       string `json:"msg"`
			Message   string `json:"message"`
			ErrDesc   string `json:"error_description"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Msg + e.Message + e.ErrDesc
		return &Error{Status: res.StatusCode, Code: e.ErrorCode, Message: msg}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// SignUp implements Client.
func (h *HTTP) SignUp(ctx context.Context, email, password string, metadata map[string]any) (*User, error) {
	var u User
	err := h.post(ctx, "/signup", map[string]any{"email": email, "password": password, "data": metadata}, &u)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// Resend implements Client.
func (h *HTTP) Resend(ctx context.Context, email string) error {
	return h.post(ctx, "/resend", map[string]any{"type": "signup", "email": email}, nil)
}

// Recover implements Client.
func (h *HTTP) Recover(ctx context.Context, email string) error {
	return h.post(ctx, "/recover", map[string]any{"email": email}, nil)
}

// SendSMS implements Client.
func (h *HTTP) SendSMS(ctx context.Context, phone string) error {
	return h.post(ctx, "/otp", map[string]any{"phone": phone, "create_user": false}, nil)
}

// Verify implements Client.
func (h *HTTP) Verify(ctx context.Context, typ, identifier, code string) (*Session, error) {
	body := map[string]any{"type": typ, "token": code}
	if typ == "sms" {
		body["phone"] = identifier
	} else {
		body["email"] = identifier
	}
	var s Session
	if err := h.post(ctx, "/verify", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
