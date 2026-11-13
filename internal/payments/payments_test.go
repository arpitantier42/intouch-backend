package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"event":"payment.captured"}`)
	sig := Sign("s3cret", body)
	if !VerifySignature("s3cret", body, sig) || !VerifySignature("s3cret", body, " "+sig+" ") {
		t.Fatal("valid signature rejected")
	}
	if VerifySignature("s3cret", append(body, ' '), sig) || VerifySignature("", body, Sign("", body)) || VerifySignature("s3cret", body, "") {
		t.Fatal("invalid signature accepted")
	}
}

func TestParseWebhookAndKey(t *testing.T) {
	p, err := ParseRazorpayWebhook([]byte(`{"event":"payment.captured","payload":{"payment":{"entity":
		{"id":"pay_1","order_id":"order_1","amount":5000,"status":"captured","notes":{"raise_id":"r","donor_id":"d"}}}}}`))
	if err != nil || !p.Captured() || p.Key() != "order_1" || p.AmountMinor != 5000 || p.RaiseID != "r" || p.DonorID != "d" {
		t.Fatalf("%+v %v", p, err)
	}
	if (Payment{Event: "payment.captured", PaymentID: "pay_2", Status: "captured"}).Key() != "pay_2" {
		t.Fatal("payment id fallback")
	}
	if (Payment{Event: "payment.failed", PaymentID: "pay_3", Status: "failed"}).Captured() {
		t.Fatal("failed payment counted as captured")
	}
}

func TestRazorpayCreateOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Path != "/orders" || user != "kid" || pass != "ksec" || in["amount"] != float64(9900) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":"order_X","amount":9900,"currency":"INR"}`))
	}))
	defer srv.Close()
	g := NewRazorpay("kid", "ksec")
	g.BaseURL = srv.URL
	o, err := g.CreateOrder(context.Background(), Order{AmountMinor: 9900, Currency: "INR", Receipt: "r1"})
	if err != nil || o.ID != "order_X" || o.KeyID != "kid" {
		t.Fatalf("%+v %v", o, err)
	}
	g.KeySecret = "wrong"
	if _, err := g.CreateOrder(context.Background(), Order{AmountMinor: 9900}); err == nil {
		t.Fatal("gateway error not reported")
	}
}
