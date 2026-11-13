package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/push"
	"github.com/jackc/pgx/v5"
)

// registerEvents adds OP-062..067 and OP-070. The rules live in SQL
// (migration ..._events_api.sql: list_events, create_event, get_event,
// event_landing, update_event, delete_event); reminders are an RLS insert.
func registerEvents(g Groups, d Deps) {
	h := &eventHandlers{d: d}
	ready := (&meHandlers{d: d}).ready
	g.User.GET("/communities/:id/events", ready, h.list)
	g.User.POST("/communities/:id/events", ready, h.create)
	g.User.GET("/events/:id", ready, h.get)
	g.Public.GET("/events/:id/join", ready, h.landing)
	g.User.PATCH("/events/:id", ready, h.update)
	g.User.DELETE("/events/:id", ready, h.remove)
	g.User.POST("/events/:id/reminders", ready, h.remind)
}

type eventHandlers struct{ d Deps }

func (h *eventHandlers) rpc(c *gin.Context, status int, sql string, args ...any) {
	claims := auth.FromContext(c)
	var out []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		return tx.QueryRow(c, sql, args...).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Data(status, "application/json; charset=utf-8", out)
}

// GET /communities/{id}/events?tab&q
func (h *eventHandlers) list(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusForbidden, "Members only")
		return
	}
	h.rpc(c, http.StatusOK, `select public.list_events($1, $2, $3)::text`,
		c.Param("id"), c.Query("tab"), c.Query("q"))
}

type eventBody struct {
	Title       *string         `json:"title"`
	Description *string         `json:"description"`
	CoverURL    *string         `json:"cover_url"`
	StartsAt    *string         `json:"starts_at"`
	EndsAt      *string         `json:"ends_at"`
	Format      *string         `json:"format"`
	Venue       *string         `json:"venue"`
	JoinURL     *string         `json:"join_url"`
	Tags        json.RawMessage `json:"tags"`
	Speaker     *string         `json:"speaker"`
	SeatCap     json.RawMessage `json:"seat_cap"`
	IsPaid      *bool           `json:"is_paid"`
	PriceMinor  json.RawMessage `json:"price_minor"`
	PaymentURL  *string         `json:"payment_url"`
}

const numbersMsg = "Seats and price must be valid positive numbers"

// positiveInt reads an optional JSON number that must be a positive integer.
// ok=false for anything else ("abc", 0, -1, 1.5).
func positiveInt(raw json.RawMessage) (v *int64, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return nil, false
	}
	i, err := n.Int64()
	if err != nil || i <= 0 {
		return nil, false
	}
	return &i, true
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

func isHTTPURL(s string) bool {
	l := strings.ToLower(s)
	return (strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")) &&
		len(strings.SplitN(l, "://", 2)[1]) > 0 && !strings.ContainsAny(s, " \t\n")
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// POST /communities/{id}/events — 201 Event (+ post_id), 400 composer
// rules (error.fields, first message as error.message), 403 not joined.
func (h *eventHandlers) create(c *gin.Context) {
	var b eventBody
	raw, _ := readBody(c)
	if raw == nil || json.Unmarshal(raw, &b) != nil {
		abortFields(c, map[string]string{"title": "Send a JSON object."}, "title")
		return
	}
	fields := map[string]string{}
	if str(b.Title) == "" {
		fields["title"] = "Add an event title"
	}
	var start, end *time.Time
	if t, err := time.Parse(time.RFC3339, str(b.StartsAt)); err == nil {
		start = &t
	} else {
		fields["starts_at"] = "Fill date, time and location"
	}
	if s := str(b.EndsAt); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			end = &t
		} else {
			fields["ends_at"] = "Enter a valid end time"
		}
	} else if start != nil {
		t := start.Add(2 * time.Hour) // end auto-fills to start + 2h
		end = &t
	}
	if start != nil && end != nil && !end.After(*start) {
		fields["ends_at"] = "End must be after start"
	}
	format := strings.ToLower(str(b.Format))
	if format == "" {
		format = "in_person"
		if str(b.Venue) == "" && str(b.JoinURL) != "" {
			format = "online"
		}
	}
	switch format {
	case "in_person":
		if str(b.Venue) == "" {
			fields["venue"] = "Fill date, time and location"
		}
	case "online":
		if j := str(b.JoinURL); j == "" {
			fields["join_url"] = "Fill date, time and location"
		} else if !isHTTPURL(j) {
			fields["join_url"] = "Enter an http(s) event link"
		}
	default:
		fields["format"] = "Choose in_person or online."
	}
	seatCap, ok := positiveInt(b.SeatCap)
	if !ok {
		fields["seat_cap"] = numbersMsg
	}
	paid := b.IsPaid != nil && *b.IsPaid
	price, ok := positiveInt(b.PriceMinor)
	if paid && (!ok || price == nil) {
		fields["price_minor"] = numbersMsg
	}
	if paid && !isHTTPURL(str(b.PaymentURL)) {
		fields["payment_url"] = "Add an http(s) payment link"
	}
	if len(fields) > 0 {
		abortFields(c, fields, "title", "starts_at", "venue", "join_url", "format", "ends_at",
			"seat_cap", "price_minor", "payment_url")
		return
	}
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusForbidden, "Members only")
		return
	}
	var tags any
	if len(b.Tags) > 0 && string(b.Tags) != "null" {
		tags = string(b.Tags)
	}
	claims := auth.FromContext(c)
	name, avatar := oauthMetadata(claims)
	var out []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, ensureProfileSQL, name, avatar); err != nil {
			return err
		}
		return tx.QueryRow(c, `select public.create_event($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb,
		                         $11, $12, $13, $14, $15)::text`,
			c.Param("id"), str(b.Title), nilIfEmpty(str(b.Description)), nilIfEmpty(str(b.CoverURL)),
			start, end, format, nilIfEmpty(str(b.Venue)), nilIfEmpty(str(b.JoinURL)), tags,
			nilIfEmpty(str(b.Speaker)), seatCap, paid, price, nilIfEmpty(str(b.PaymentURL))).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Data(http.StatusCreated, "application/json; charset=utf-8", out)
}

// GET /events/{id} — 200 detail (join_url only for the creator or a
// confirmed seat), 403 non-member, 404 unknown/deleted.
func (h *eventHandlers) get(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		abortNotFound(c)
		return
	}
	h.rpc(c, http.StatusOK, `select public.get_event($1)::text`, c.Param("id"))
}

// GET /events/{id}/join — public preview; 403 private community, 404 invalid.
func (h *eventHandlers) landing(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, "Event not found")
		return
	}
	var out []byte
	fn := func(tx pgx.Tx) error {
		return tx.QueryRow(c, `select public.event_landing($1)::text`, c.Param("id")).Scan(&out)
	}
	var err error
	if claims := auth.FromContext(c); claims != nil {
		err = h.d.DB.AsUser(c, claims.Raw, fn) // fills "joined" for signed-in members
	} else {
		err = h.d.DB.AsAnon(c, fn)
	}
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}

// PATCH /events/{id} — creator only (403), CHECK failures → 400.
func (h *eventHandlers) update(c *gin.Context) {
	var patch map[string]json.RawMessage
	if !bodyObject(c, &patch) {
		return
	}
	if !uuidRe.MatchString(c.Param("id")) {
		abortNotFound(c)
		return
	}
	if v, ok := patch["seat_cap"]; ok {
		if _, valid := positiveInt(v); !valid {
			abortFields(c, map[string]string{"seat_cap": numbersMsg}, "seat_cap")
			return
		}
	}
	if v, ok := patch["price_minor"]; ok {
		if _, valid := positiveInt(v); !valid {
			abortFields(c, map[string]string{"price_minor": numbersMsg}, "price_minor")
			return
		}
	}
	js, _ := json.Marshal(patch)
	h.rpc(c, http.StatusOK, `select public.update_event($1, $2::jsonb)::text`, c.Param("id"), string(js))
}

// DELETE /events/{id} — 204; going attendees get an in-app notification
// (in SQL) and a push (after commit).
func (h *eventHandlers) remove(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		abortNotFound(c)
		return
	}
	claims := auth.FromContext(c)
	var out []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		return tx.QueryRow(c, `select public.delete_event($1)::text`, c.Param("id")).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	var res struct {
		ID       string   `json:"id"`
		Title    string   `json:"title"`
		Notified []string `json:"notified"`
	}
	_ = json.Unmarshal(out, &res)
	if h.d.Push != nil && len(res.Notified) > 0 {
		n := push.Notification{
			UserIDs: res.Notified,
			Title:   "Event cancelled",
			Body:    res.Title + " was cancelled by the host.",
			Data:    map[string]string{"kind": "event_deleted", "event_id": res.ID},
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = h.d.Push.Send(ctx, n)
		}()
	}
	c.Status(http.StatusNoContent)
}

// POST /events/{id}/reminders — "Remind me". Body {channel push|email
// (default push), send_at (default 1 hour before start)}. 201 Reminder;
// repeating returns the existing row. 400 bad channel/time, 404 not visible.
func (h *eventHandlers) remind(c *gin.Context) {
	var b struct {
		Channel string `json:"channel"`
		SendAt  string `json:"send_at"`
	}
	if raw, _ := readBody(c); raw != nil && len(bytes.TrimSpace(raw)) > 0 {
		if json.Unmarshal(raw, &b) != nil {
			apierr.Abort(c, http.StatusBadRequest, "Send a JSON object.")
			return
		}
	}
	channel := strings.ToLower(strings.TrimSpace(b.Channel))
	if channel == "" {
		channel = "push"
	}
	fields := map[string]string{}
	if channel != "push" && channel != "email" {
		fields["channel"] = "Choose push or email."
	}
	var sendAt *time.Time
	if s := strings.TrimSpace(b.SendAt); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			fields["send_at"] = "Enter a valid time."
		} else if !t.After(time.Now()) {
			fields["send_at"] = "Pick a time in the future."
		} else {
			sendAt = &t
		}
	}
	if len(fields) > 0 {
		abortFields(c, fields, "channel", "send_at")
		return
	}
	if !uuidRe.MatchString(c.Param("id")) {
		abortNotFound(c)
		return
	}
	claims := auth.FromContext(c)
	var out []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		var starts time.Time
		if err := tx.QueryRow(c, `select starts_at from public.events where id = $1`, c.Param("id")).Scan(&starts); err != nil {
			return err // RLS: not joined, deleted or unknown → 404
		}
		if !starts.After(time.Now()) {
			return apierr.New(http.StatusBadRequest, "This event has already started.")
		}
		at := starts.Add(-time.Hour)
		if sendAt != nil {
			at = *sendAt
		}
		if !at.After(time.Now()) {
			at = time.Now().Add(time.Minute)
		}
		if at.After(starts) {
			return apierr.New(http.StatusBadRequest, "Pick a time before the event starts.")
		}
		at = at.UTC().Truncate(time.Second)
		const cols = `id, user_id, event_id, channel, send_at, sent_at`
		err := tx.QueryRow(c, `insert into public.reminders as r (event_id, channel, send_at) values ($1, $2, $3)
		                       on conflict on constraint reminders_slot_key do nothing
		                       returning to_jsonb(r.*)::text`, c.Param("id"), channel, at).Scan(&out)
		if err == pgx.ErrNoRows {
			return tx.QueryRow(c, `select to_jsonb(r.*)::text from (select `+cols+` from public.reminders
			                       where event_id = $1 and user_id = auth.uid() and channel = $2 and send_at = $3) r`,
				c.Param("id"), channel, at).Scan(&out)
		}
		return err
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Data(http.StatusCreated, "application/json; charset=utf-8", out)
}
