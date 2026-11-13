package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/mailer"
	"github.com/intouch/api/internal/payments"
	"github.com/jackc/pgx/v5"
)

// registerWebhooks adds OP-093 POST /internal/webhooks/{provider}. The
// Internal group already requires X-API-Key; each provider body is also
// HMAC-signed (hex SHA-256 of the raw body):
//
//	razorpay — X-Razorpay-Signature, key RAZORPAY_WEBHOOK_SECRET
//	email    — X-Webhook-Signature,  key EMAIL_WEBHOOK_SECRET
//
// A missing secret or a bad signature is 401. Every valid delivery is 200
// with {"status": applied|duplicate|ignored, ...} so the provider stops retrying.
func registerWebhooks(g Groups, d Deps) {
	h := &webhookHandlers{d: d}
	g.Internal.POST("/webhooks/:provider", (&meHandlers{d: d}).ready, h.receive)
}

type webhookHandlers struct{ d Deps }

func (h *webhookHandlers) receive(c *gin.Context) {
	body, err := readBody(c)
	if err != nil || body == nil {
		body = []byte{}
	}
	switch c.Param("provider") {
	case "razorpay":
		if !payments.VerifySignature(h.d.Config.RazorpayWebhook, body, c.GetHeader("X-Razorpay-Signature")) {
			apierr.Abort(c, http.StatusUnauthorized, "Invalid signature")
			return
		}
		h.razorpay(c, body)
	case "email":
		if !payments.VerifySignature(h.d.Config.EmailWebhook, body, c.GetHeader("X-Webhook-Signature")) {
			apierr.Abort(c, http.StatusUnauthorized, "Invalid signature")
			return
		}
		h.email(c, body)
	default:
		apierr.Abort(c, http.StatusNotFound, "Unknown webhook provider")
	}
}

// razorpay applies payment.captured / order.paid once per order (or payment)
// id; the first application emails the donor a receipt after commit.
func (h *webhookHandlers) razorpay(c *gin.Context, body []byte) {
	p, err := payments.ParseRazorpayWebhook(body)
	if err != nil {
		apierr.Abort(c, http.StatusBadRequest, "Malformed webhook body")
		return
	}
	if !p.Captured() {
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "event": p.Event})
		return
	}
	var cid *string
	var applied bool
	err = h.d.DB.AsService(c, func(tx pgx.Tx) error {
		err := tx.QueryRow(c, `select contribution_id::text, applied
			from public.capture_gateway_payment($1, $2, $3, $4)`,
			p.Key(), uuidOrNil(p.RaiseID), uuidOrNil(p.DonorID), p.AmountMinor).Scan(&cid, &applied)
		if err == pgx.ErrNoRows {
			return nil
		}
		return err
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	if cid == nil {
		log.Printf("razorpay webhook: unknown contribution for %s", p.Key())
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "event": p.Event})
		return
	}
	if !applied {
		c.JSON(http.StatusOK, gin.H{"status": "duplicate", "contribution_id": *cid})
		return
	}
	go h.sendReceipt(*cid)
	c.JSON(http.StatusOK, gin.H{"status": "applied", "contribution_id": *cid})
}

func uuidOrNil(s string) any {
	if uuidRe.MatchString(s) {
		return s
	}
	return nil
}

func (h *webhookHandlers) sendReceipt(contributionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var r struct {
		Email        *string `json:"email"`
		DonorName    string  `json:"donor_name"`
		RaiseTitle   string  `json:"raise_title"`
		ReceiverName string  `json:"receiver_name"`
		AmountLabel  string  `json:"amount_label"`
		PaymentID    string  `json:"gateway_payment_id"`
		CreatedAt    string  `json:"created_at"`
	}
	var raw []byte
	if err := h.d.DB.AsService(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select public.contribution_receipt($1)::text`, contributionID).Scan(&raw)
	}); err != nil || raw == nil || json.Unmarshal(raw, &r) != nil {
		log.Printf("receipt %s: %v", contributionID, err)
		return
	}
	if r.Email == nil || *r.Email == "" {
		log.Printf("receipt %s: donor has no email", contributionID)
		return
	}
	msg := mailer.Message{
		To:      *r.Email,
		Subject: fmt.Sprintf("Receipt: %s to %s", r.AmountLabel, r.RaiseTitle),
		Text: fmt.Sprintf("Hi %s,\n\nThank you for supporting %q.\n\nAmount: %s\nPaid to: %s\nReference: %s\nReceipt id: %s\n",
			r.DonorName, r.RaiseTitle, r.AmountLabel, r.ReceiverName, r.PaymentID, contributionID),
	}
	if err := h.d.Mailer.Send(ctx, msg); err != nil {
		log.Printf("receipt %s: %v", contributionID, err)
		return
	}
	if err := h.d.DB.AsService(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `select public.mark_receipt_sent($1)`, contributionID)
		return err
	}); err != nil {
		log.Printf("mark receipt %s: %v", contributionID, err)
	}
}

// bounceTypes are the event names treated as a permanent delivery failure.
var bounceTypes = map[string]bool{"bounce": true, "bounced": true, "email.bounced": true,
	"hard_bounce": true, "dropped": true, "failed": true, "email.failed": true}

type emailEvent struct {
	Type         string            `json:"type"`
	Event        string            `json:"event"`
	Email        string            `json:"email"`
	Recipient    string            `json:"recipient"`
	InvitationID string            `json:"invitation_id"`
	Metadata     map[string]string `json:"metadata"`
}

// email marks invitations failed for bounce events. The body is one event
// object or an array of them: {type|event, invitation_id | metadata.invitation_id,
// email|recipient}. With an invitation id only that invitation is marked;
// otherwise every email invitation still 'sent' to that address.
func (h *webhookHandlers) email(c *gin.Context, body []byte) {
	var events []emailEvent
	if err := json.Unmarshal(body, &events); err != nil {
		var one emailEvent
		if err := json.Unmarshal(body, &one); err != nil {
			apierr.Abort(c, http.StatusBadRequest, "Malformed webhook body")
			return
		}
		events = []emailEvent{one}
	}
	marked := 0
	err := h.d.DB.AsService(c, func(tx pgx.Tx) error {
		for _, ev := range events {
			kind := strings.ToLower(ev.Type)
			if kind == "" {
				kind = strings.ToLower(ev.Event)
			}
			if !bounceTypes[kind] {
				continue
			}
			inv := ev.InvitationID
			if inv == "" {
				inv = ev.Metadata["invitation_id"]
			}
			addr := strings.TrimSpace(ev.Email)
			if addr == "" {
				addr = strings.TrimSpace(ev.Recipient)
			}
			var n int
			var err error
			switch {
			case uuidRe.MatchString(inv):
				var ok bool
				err = tx.QueryRow(c, `select public.mark_invitation_failed($1)`, inv).Scan(&ok)
				if ok {
					n = 1
				}
			case addr != "":
				err = tx.QueryRow(c, `select public.mark_invitations_failed_for_email($1)`, addr).Scan(&n)
			}
			if err != nil {
				return err
			}
			marked += n
		}
		return nil
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	status := "applied"
	if marked == 0 {
		status = "ignored"
	}
	c.JSON(http.StatusOK, gin.H{"status": status, "marked": marked})
}
