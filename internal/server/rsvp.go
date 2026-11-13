package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/mailer"
	"github.com/intouch/api/internal/push"
	"github.com/jackc/pgx/v5"
)

// registerRsvp adds OP-068 (setRsvp) and OP-069 (verifyRsvpPayment). The
// seat lock, payment rules and reminder scheduling live in SQL (set_rsvp,
// verify_rsvp_payment in migration ..._rsvp.sql, plus the rsvps triggers).
func registerRsvp(g Groups, d Deps) {
	h := &rsvpHandlers{d: d}
	ready := (&meHandlers{d: d}).ready
	g.User.PUT("/events/:id/rsvp", ready, h.setRsvp)
	g.User.POST("/events/:id/rsvps/:userId/verify", ready, h.verifyPayment)
}

type rsvpHandlers struct{ d Deps }

var (
	rsvpStatuses = map[string]bool{"going": true, "not_sure": true, "declined": true}
	rsvpPayments = map[string]bool{"": true, "none": true, "pay_later": true, "verification_pending": true}
	rsvpReasons  = map[string]bool{"": true, "schedule": true, "not_relevant": true, "too_far": true, "next_time": true, "other": true}
)

type rsvpBody struct {
	Status        string  `json:"status"`
	PaymentStatus string  `json:"payment_status"`
	DeclineReason *string `json:"decline_reason"`
	Note          *string `json:"note"`
	RemindPush    *bool   `json:"remind_push"`
	RemindEmail   *bool   `json:"remind_email"`
}

// rsvpEvent is the event summary set_rsvp returns under "event".
type rsvpEvent struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	Format   string    `json:"format"`
	Venue    *string   `json:"venue"`
}

// PUT /events/{id}/rsvp — 200 Rsvp + badge, seats_left(_label), join_url
// (confirmed seats only), event, calendar links. 400 bad fields, 403 not
// joined, 404 unknown event, 409 'Event full'.
func (h *rsvpHandlers) setRsvp(c *gin.Context) {
	var b rsvpBody
	raw, _ := readBody(c)
	if raw != nil {
		if err := json.Unmarshal(raw, &b); err != nil {
			abortFields(c, map[string]string{"status": "Send a JSON body."}, "status")
			return
		}
	}
	status := strings.ToLower(strings.TrimSpace(b.Status))
	pay := strings.ToLower(strings.TrimSpace(b.PaymentStatus))
	reason := ""
	if b.DeclineReason != nil {
		reason = strings.ToLower(strings.TrimSpace(*b.DeclineReason))
	}
	fields := map[string]string{}
	if !rsvpStatuses[status] {
		fields["status"] = "Choose going, not_sure or declined."
	}
	if pay == "verified" {
		fields["payment_status"] = "Only the host can verify a payment."
	} else if !rsvpPayments[pay] {
		fields["payment_status"] = "Choose pay_later or verification_pending."
	}
	if !rsvpReasons[reason] {
		fields["decline_reason"] = "Choose schedule, not_relevant, too_far, next_time or other."
	}
	if b.Note != nil && len([]rune(*b.Note)) > 500 {
		fields["note"] = "Note must be 500 characters or fewer."
	}
	if len(fields) > 0 {
		abortFields(c, fields, "status", "payment_status", "decline_reason", "note")
		return
	}
	if !uuidRe.MatchString(c.Param("id")) {
		abortNotFound(c)
		return
	}
	var payArg, reasonArg any
	if pay != "" {
		payArg = pay
	}
	if reason != "" {
		reasonArg = reason
	}
	claims := auth.FromContext(c)
	name, avatar := oauthMetadata(claims)
	var out []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, ensureProfileSQL, name, avatar); err != nil {
			return err
		}
		return tx.QueryRow(c, `select public.set_rsvp($1, $2, $3, $4, $5, $6, $7)::text`,
			c.Param("id"), status, payArg, reasonArg, b.Note, b.RemindPush, b.RemindEmail).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	body, ev, sendMail := withCalendar(out)
	if sendMail && h.d.Mailer != nil {
		if to := tokenEmail(claims); to != "" && ev != nil {
			msg := confirmationEmail(to, ev, body)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_ = h.d.Mailer.Send(ctx, msg)
			}()
		}
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// POST /events/{id}/rsvps/{userId}/verify — host marks the payment
// verified; 200 Rsvp (seat confirmed), 403 not the creator, 404 unknown
// event/RSVP, 409 not going or 'Event full'. The attendee gets a push.
func (h *rsvpHandlers) verifyPayment(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) || !uuidRe.MatchString(c.Param("userId")) {
		abortNotFound(c)
		return
	}
	claims := auth.FromContext(c)
	var out []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		return tx.QueryRow(c, `select public.verify_rsvp_payment($1, $2)::text`,
			c.Param("id"), c.Param("userId")).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	body, ev, _ := withCalendar(out)
	if h.d.Push != nil && ev != nil {
		n := push.Notification{
			UserIDs: []string{c.Param("userId")},
			Title:   "You're in",
			Body:    "Your payment for " + ev.Title + " was verified. Your seat is confirmed.",
			Data:    map[string]string{"kind": "rsvp_verified", "event_id": ev.ID},
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = h.d.Push.Send(ctx, n)
		}()
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// withCalendar adds "calendar" {google, outlook, ics} to the RPC's JSON and
// returns the event summary and the confirmation_email flag.
func withCalendar(raw []byte) ([]byte, *rsvpEvent, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw, nil, false
	}
	var ev rsvpEvent
	if err := json.Unmarshal(obj["event"], &ev); err != nil || ev.ID == "" {
		return raw, nil, false
	}
	var joinURL string
	_ = json.Unmarshal(obj["join_url"], &joinURL)
	var send bool
	_ = json.Unmarshal(obj["confirmation_email"], &send)
	cal, _ := json.Marshal(calendarLinks(ev, joinURL))
	obj["calendar"] = cal
	out, err := json.Marshal(obj)
	if err != nil {
		return raw, &ev, send
	}
	return out, &ev, send
}

// calendarLinks builds Add to calendar targets: a Google Calendar template
// URL, an Outlook web compose URL and an iCalendar body (Apple Calendar).
// The location is the venue, or the join link once the seat is confirmed.
func calendarLinks(ev rsvpEvent, joinURL string) map[string]string {
	loc := ""
	if ev.Venue != nil {
		loc = *ev.Venue
	} else if joinURL != "" {
		loc = joinURL
	}
	const gfmt = "20060102T150405Z"
	start, end := ev.StartsAt.UTC(), ev.EndsAt.UTC()
	g := url.Values{}
	g.Set("action", "TEMPLATE")
	g.Set("text", ev.Title)
	g.Set("dates", start.Format(gfmt)+"/"+end.Format(gfmt))
	if loc != "" {
		g.Set("location", loc)
	}
	o := url.Values{}
	o.Set("path", "/calendar/action/compose")
	o.Set("rru", "addevent")
	o.Set("subject", ev.Title)
	o.Set("startdt", start.Format(time.RFC3339))
	o.Set("enddt", end.Format(time.RFC3339))
	if loc != "" {
		o.Set("location", loc)
	}
	esc := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`)
	ics := strings.Join([]string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//InTouch//Events//EN",
		"BEGIN:VEVENT",
		"UID:" + ev.ID + "@intouch",
		"DTSTAMP:" + time.Now().UTC().Format(gfmt),
		"DTSTART:" + start.Format(gfmt),
		"DTEND:" + end.Format(gfmt),
		"SUMMARY:" + esc.Replace(ev.Title),
		"LOCATION:" + esc.Replace(loc),
		"END:VEVENT", "END:VCALENDAR", "",
	}, "\r\n")
	return map[string]string{
		"google":  "https://calendar.google.com/calendar/render?" + g.Encode(),
		"outlook": "https://outlook.live.com/calendar/0/deeplink/compose?" + o.Encode(),
		"ics":     ics,
	}
}

// tokenEmail is the email claim Supabase puts in access tokens.
func tokenEmail(claims *auth.Claims) string {
	var p struct {
		Email string `json:"email"`
	}
	_ = json.Unmarshal(claims.Raw, &p)
	return strings.TrimSpace(p.Email)
}

func confirmationEmail(to string, ev *rsvpEvent, body []byte) mailer.Message {
	var r struct {
		Badge   string  `json:"badge"`
		JoinURL *string `json:"join_url"`
		Event   struct {
			PaymentURL *string `json:"payment_url"`
		} `json:"event"`
	}
	_ = json.Unmarshal(body, &r)
	ist := time.FixedZone("IST", 5*3600+1800)
	var sb strings.Builder
	subject := "You're in: " + ev.Title
	if r.Badge != "Going" {
		subject = "RSVP received: " + ev.Title
	}
	fmt.Fprintf(&sb, "%s\n%s – %s (IST)\n", ev.Title,
		ev.StartsAt.In(ist).Format("Mon 2 Jan 2006, 3:04 PM"), ev.EndsAt.In(ist).Format("3:04 PM"))
	if ev.Venue != nil {
		fmt.Fprintf(&sb, "Where: %s\n", *ev.Venue)
	}
	switch {
	case r.JoinURL != nil && *r.JoinURL != "":
		fmt.Fprintf(&sb, "Join link: %s\n", *r.JoinURL)
	case r.Badge == "Payment pending" && r.Event.PaymentURL != nil:
		fmt.Fprintf(&sb, "Pay to confirm your seat: %s\n", *r.Event.PaymentURL)
	case r.Badge == "Verification pending":
		sb.WriteString("Your payment is being verified by the host.\n")
	}
	return mailer.Message{To: to, Subject: subject, Text: sb.String()}
}
