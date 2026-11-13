package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/push"
	"github.com/jackc/pgx/v5"
)

// maxChatAttachment is the 25 MB cap on chat attachments (DB CHECK too).
const maxChatAttachment = 26214400

// registerMessages adds OP-050..054: paging + mark read, send (+ push),
// edit, delete for me/everyone, react/star/pin. Rules live in the SQL RPCs
// (migration ..._messaging.sql); Realtime broadcasts the committed rows.
func registerMessages(g Groups, d Deps) {
	m := &messageHandlers{h: &admissionHandlers{d: d}, d: d}
	ready := (&meHandlers{d: d}).ready
	g.User.GET("/threads/:id/messages", ready, m.list)
	g.User.POST("/threads/:id/messages", ready, m.send)
	g.User.PATCH("/messages/:id", ready, m.edit)
	g.User.DELETE("/messages/:id", ready, m.remove)
	g.User.PUT("/messages/:id/marks", ready, m.mark)
}

type messageHandlers struct {
	h *admissionHandlers
	d Deps
}

// GET /threads/{id}/messages?view&q&after (RFC 3339)&before&limit
func (m *messageHandlers) list(c *gin.Context) {
	fields := map[string]string{}
	parseTime := func(k string) any {
		s := strings.TrimSpace(c.Query(k))
		if s == "" {
			return nil
		}
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			fields[k] = k + " must be an RFC 3339 time."
			return nil
		}
		return ts
	}
	after, before := parseTime("after"), parseTime("before")
	var limit any
	if s := strings.TrimSpace(c.Query("limit")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			fields["limit"] = "limit must be a positive number."
		} else {
			limit = n
		}
	}
	view := strings.ToLower(strings.TrimSpace(c.DefaultQuery("view", "all")))
	switch view {
	case "", "all", "pinned", "starred", "media", "links", "documents":
	default:
		fields["view"] = "View must be all, pinned, starred, media, links or documents."
	}
	if len(fields) > 0 {
		abortFields(c, fields, "view", "after", "before", "limit")
		return
	}
	m.h.rpcJSON(c, http.StatusOK, `select public.list_messages($1, $2, $3, $4, $5, $6)::text`,
		c.Param("id"), view, c.Query("q"), after, before, limit)
}

// scopeHeader reads the client's active community (header scope_community_id;
// Scope-Community-Id / X-Scope-Community-Id are accepted because proxies
// may drop header names with underscores).
func scopeHeader(c *gin.Context) string {
	for _, h := range []string{"scope_community_id", "Scope-Community-Id", "X-Scope-Community-Id"} {
		if v := strings.TrimSpace(c.GetHeader(h)); v != "" {
			return v
		}
	}
	return ""
}

type sendBody struct {
	Kind            string  `json:"kind"`
	Body            *string `json:"body"`
	AttachmentURL   *string `json:"attachment_url"`
	AttachmentBytes *int64  `json:"attachment_bytes"`
	ReplyToID       *string `json:"reply_to_id"`
	Forwarded       bool    `json:"forwarded"`
	ForwardOfID     *string `json:"forward_of_id"`
}

// POST /threads/{id}/messages — 201 message; 400 whitespace-only text;
// 403 out of scope / blocked / peer not joined; 413 attachment > 25 MB.
func (m *messageHandlers) send(c *gin.Context) {
	var b sendBody
	raw, err := readBody(c)
	if err != nil || raw == nil || json.Unmarshal(raw, &b) != nil {
		abortFields(c, map[string]string{"body": "Send the message as JSON."}, "body")
		return
	}
	scope := scopeHeader(c)
	fields := map[string]string{}
	if !uuidRe.MatchString(scope) {
		fields["scope_community_id"] = "Choose a community to send from."
	}
	b.Kind = strings.ToLower(strings.TrimSpace(b.Kind))
	if b.Kind == "" {
		b.Kind = "text"
	}
	if b.ForwardOfID == nil && (b.Kind == "text" || b.Kind == "location" || b.Kind == "contact") &&
		(b.Body == nil || strings.TrimSpace(*b.Body) == "") {
		fields["body"] = "Type a message."
	}
	for k, v := range map[string]*string{"reply_to_id": b.ReplyToID, "forward_of_id": b.ForwardOfID} {
		if v != nil && !uuidRe.MatchString(*v) {
			fields[k] = k + " must be a message id."
		}
	}
	if len(fields) > 0 {
		abortFields(c, fields, "scope_community_id", "body", "reply_to_id", "forward_of_id")
		return
	}
	if b.AttachmentBytes != nil && *b.AttachmentBytes > maxChatAttachment {
		apierr.Abort(c, http.StatusRequestEntityTooLarge, "Attachments can be at most 25 MB.")
		return
	}

	claims := auth.FromContext(c)
	var out []byte
	var n *push.Notification
	err = m.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		if err := tx.QueryRow(c,
			`select public.send_message($1, $2, $3, $4, $5, $6, $7, $8, $9)::text`,
			c.Param("id"), scope, b.Kind, b.Body, b.AttachmentURL, b.AttachmentBytes,
			b.ReplyToID, b.Forwarded, b.ForwardOfID).Scan(&out); err != nil {
			return err
		}
		var msg struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(out, &msg)
		n, err = pushFor(c, tx, msg.ID)
		return err
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	if n != nil && len(n.UserIDs) > 0 {
		go func(n push.Notification) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = m.d.Push.Send(ctx, n)
		}(*n)
	}
	c.Data(http.StatusCreated, "application/json; charset=utf-8", out)
}

// pushFor builds the alert for a just-sent message: unmuted, unblocked,
// still-joined participants other than the sender.
func pushFor(ctx context.Context, tx pgx.Tx, messageID string) (*push.Notification, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `select public.message_push($1)::text`, messageID).Scan(&raw); err != nil {
		return nil, err
	}
	var p struct {
		UserIDs  []string `json:"user_ids"`
		ThreadID string   `json:"thread_id"`
		Title    string   `json:"title"`
		Body     string   `json:"body"`
	}
	if raw == nil || json.Unmarshal(raw, &p) != nil {
		return nil, nil
	}
	return &push.Notification{UserIDs: p.UserIDs, Title: p.Title, Body: p.Body,
		Data: map[string]string{"kind": "chat_message", "thread_id": p.ThreadID, "message_id": messageID}}, nil
}

// PATCH /messages/{id} {"body"} — blank body leaves the message unchanged.
func (m *messageHandlers) edit(c *gin.Context) {
	var b struct {
		Body *string `json:"body"`
	}
	raw, err := readBody(c)
	if err != nil || (raw != nil && json.Unmarshal(raw, &b) != nil) {
		abortFields(c, map[string]string{"body": "Send the edit as JSON."}, "body")
		return
	}
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, "Message not found.")
		return
	}
	m.h.rpcJSON(c, http.StatusOK, `select public.edit_message($1, $2)::text`, c.Param("id"), b.Body)
}

// DELETE /messages/{id}?scope=me|everyone — 204.
func (m *messageHandlers) remove(c *gin.Context) {
	scope := strings.ToLower(strings.TrimSpace(c.Query("scope")))
	if scope != "me" && scope != "everyone" {
		abortFields(c, map[string]string{"scope": "Scope must be me or everyone."}, "scope")
		return
	}
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, "Message not found.")
		return
	}
	m.h.rpcNoContent(c, `select public.delete_message($1, $2)`, c.Param("id"), scope)
}

// PUT /messages/{id}/marks?action=react|unreact|star|unstar|pin|unpin {"emoji"}
func (m *messageHandlers) mark(c *gin.Context) {
	action := strings.ToLower(strings.TrimSpace(c.Query("action")))
	var b struct {
		Emoji  *string `json:"emoji"`
		Action string  `json:"action"`
	}
	if raw, err := readBody(c); err == nil && raw != nil {
		_ = json.Unmarshal(raw, &b)
	}
	if action == "" {
		action = strings.ToLower(strings.TrimSpace(b.Action))
	}
	fields := map[string]string{}
	switch action {
	case "react":
		if b.Emoji == nil || strings.TrimSpace(*b.Emoji) == "" {
			fields["emoji"] = "Choose an emoji to react with."
		} else if len([]rune(strings.TrimSpace(*b.Emoji))) > 16 {
			fields["emoji"] = "Emoji must be at most 16 characters."
		}
	case "unreact", "star", "unstar", "pin", "unpin":
	default:
		fields["action"] = "Action must be react, unreact, star, unstar, pin or unpin."
	}
	if len(fields) > 0 {
		abortFields(c, fields, "action", "emoji")
		return
	}
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, "Message not found.")
		return
	}
	m.h.rpcJSON(c, http.StatusOK, `select public.mark_message($1, $2, $3)::text`, c.Param("id"), action, b.Emoji)
}
