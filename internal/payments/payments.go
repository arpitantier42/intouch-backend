// Package payments is the one adapter between the API and the payment
// gateway (Razorpay). The gateway's order and webhook shapes are assumed from
// its public docs and may change when payments are switched on; keep every
// gateway detail inside this package.
package payments

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Order is a checkout the client completes with the gateway's SDK.
type Order struct {
	AmountMinor int64             // paise
	Currency    string            // "INR"
	Receipt     string            // our reference (≤ 40 chars)
	Notes       map[string]string // copied onto the order (raise_id, donor_id)
}

// CreatedOrder is the gateway's answer.
type CreatedOrder struct {
	ID          string // order id, e.g. order_Abc123
	AmountMinor int64
	Currency    string
	KeyID       string // public key id the client SDK needs
}

// Gateway creates orders. nil in server.Deps means "no gateway configured".
type Gateway interface {
	CreateOrder(ctx context.Context, o Order) (CreatedOrder, error)
	Provider() string
}

// Razorpay is the HTTP Gateway.
type Razorpay struct {
	KeyID, KeySecret string
	BaseURL          string // default https://api.razorpay.com/v1
	HTTP             *http.Client
}

// NewRazorpay returns the gateway for the given API keys.
func NewRazorpay(keyID, keySecret string) *Razorpay {
	return &Razorpay{KeyID: keyID, KeySecret: keySecret, BaseURL: "https://api.razorpay.com/v1",
		HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Provider implements Gateway.
func (r *Razorpay) Provider() string { return "razorpay" }

// Error is a gateway failure; Status is the gateway's HTTP status (0 = transport).
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return fmt.Sprintf("payment gateway: %d %s", e.Status, e.Msg) }

// CreateOrder implements Gateway (POST /orders).
func (r *Razorpay) CreateOrder(ctx context.Context, o Order) (CreatedOrder, error) {
	body, _ := json.Marshal(map[string]any{
		"amount": o.AmountMinor, "currency": o.Currency, "receipt": o.Receipt, "notes": o.Notes,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.BaseURL, "/")+"/orders", bytes.NewReader(body))
	if err != nil {
		return CreatedOrder{}, &Error{Msg: err.Error()}
	}
	req.SetBasicAuth(r.KeyID, r.KeySecret)
	req.Header.Set("Content-Type", "application/json")
	res, err := r.HTTP.Do(req)
	if err != nil {
		return CreatedOrder{}, &Error{Msg: "request failed"}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		return CreatedOrder{}, &Error{Status: res.StatusCode, Msg: http.StatusText(res.StatusCode)}
	}
	var out struct {
		ID       string `json:"id"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		return CreatedOrder{}, &Error{Status: res.StatusCode, Msg: "malformed order"}
	}
	return CreatedOrder{ID: out.ID, AmountMinor: out.Amount, Currency: out.Currency, KeyID: r.KeyID}, nil
}

// Sign returns the hex HMAC-SHA256 of body with secret — the scheme of
// Razorpay's X-Razorpay-Signature (and of our email-provider webhook).
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// VerifySignature reports whether sig is the HMAC of body. An empty secret
// never verifies.
func VerifySignature(secret string, body []byte, sig string) bool {
	if secret == "" || sig == "" {
		return false
	}
	want := Sign(secret, body)
	return hmac.Equal([]byte(want), []byte(strings.ToLower(strings.TrimSpace(sig))))
}

// Payment is the captured payment a webhook reports.
type Payment struct {
	Event       string
	PaymentID   string
	OrderID     string
	AmountMinor int64
	Status      string
	RaiseID     string // from notes
	DonorID     string // from notes
}

// Key is the idempotency key: the order id (one order = one contribution),
// else the payment id.
func (p Payment) Key() string {
	if p.OrderID != "" {
		return p.OrderID
	}
	return p.PaymentID
}

// Captured reports whether the event means money was captured.
func (p Payment) Captured() bool {
	return (p.Event == "payment.captured" || p.Event == "order.paid") && p.Status == "captured" && p.Key() != ""
}

// ParseRazorpayWebhook reads a Razorpay webhook body
// ({event, payload: {payment: {entity: {...}}}}).
func ParseRazorpayWebhook(body []byte) (Payment, error) {
	var w struct {
		Event   string `json:"event"`
		Payload struct {
			Payment struct {
				Entity struct {
					ID      string         `json:"id"`
					OrderID string         `json:"order_id"`
					Amount  int64          `json:"amount"`
					Status  string         `json:"status"`
					Notes   map[string]any `json:"notes"`
				} `json:"entity"`
			} `json:"payment"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return Payment{}, err
	}
	e := w.Payload.Payment.Entity
	note := func(k string) string { s, _ := e.Notes[k].(string); return s }
	return Payment{Event: w.Event, PaymentID: e.ID, OrderID: e.OrderID, AmountMinor: e.Amount,
		Status: e.Status, RaiseID: note("raise_id"), DonorID: note("donor_id")}, nil
}
