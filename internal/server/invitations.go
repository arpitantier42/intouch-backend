package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/mailer"
	"github.com/intouch/api/internal/profile"
	"github.com/jackc/pgx/v5"
)

// invalidInviteMsg is the landing page's message for every unusable code.
const invalidInviteMsg = "This invitation is not valid. Ask a community admin for an invite."

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// registerInvitations adds OP-025..028. Rules live in SQL: create_invitations
// and revoke_invitation (migration ..._invitations.sql), open_invitation and
// accept_invitation (identity migration).
func registerInvitations(g Groups, d Deps) {
	h := &invitationHandlers{d: d}
	ready := (&meHandlers{d: d}).ready
	g.User.POST("/communities/:id/invitations", ready, h.create)
	g.Public.GET("/communities/:id/invitations/:code", ready, h.open)
	g.User.POST("/communities/:id/invitations/:code/accept", ready, h.accept)
	g.User.DELETE("/communities/:id/invitations/:code", ready, h.revoke)
}

type invitationHandlers struct{ d Deps }

// createBody is the createInvitations request: an Invitation (single
// recipient) or, for batches, the plural lists.
type createBody struct {
	Channel        string     `json:"channel"`
	InviteeEmail   *string    `json:"invitee_email"`
	InviteeUserID  *string    `json:"invitee_user_id"`
	Emails         []string   `json:"emails"`
	InviteeUserIDs []string   `json:"invitee_user_ids"`
	ExpiresAt      *time.Time `json:"expires_at"`
	MaxUses        *int       `json:"max_uses"`
	Note           string     `json:"note"`
}

type issuedInvitation struct {
	ID           string  `json:"id"`
	Code         string  `json:"code"`
	InviteeEmail *string `json:"invitee_email"`
}

// POST /communities/{id}/invitations — 201 with the invitation (single
// recipient) or a list (emails / invitee_user_ids). 400 flags malformed
// emails (nothing is created or sent), self or existing members; 403 when
// the caller's role is not allowed by invite_permission.
func (h *invitationHandlers) create(c *gin.Context) {
	var b createBody
	raw, err := readBody(c)
	if err != nil || raw == nil || json.Unmarshal(raw, &b) != nil {
		abortFields(c, map[string]string{"channel": "Choose link, whatsapp, email or in_app."}, "channel")
		return
	}
	b.Channel = strings.ToLower(strings.TrimSpace(b.Channel))
	list := len(b.Emails) > 0 || len(b.InviteeUserIDs) > 0

	var emails []string
	var users []string
	switch b.Channel {
	case "link", "whatsapp":
	case "email":
		key := "emails"
		emails = b.Emails
		if !list && b.InviteeEmail != nil {
			key, emails = "invitee_email", []string{*b.InviteeEmail}
		}
		if len(emails) == 0 {
			abortFields(c, map[string]string{"emails": "Add at least one email address."}, "emails")
			return
		}
		fields := map[string]string{}
		var order []string
		for i, e := range emails {
			e = strings.TrimSpace(e)
			emails[i] = e
			if !profile.ValidateEmail(e) {
				k := key
				if key == "emails" {
					k = fmt.Sprintf("emails[%d]", i)
				}
				fields[k] = fmt.Sprintf("%q is not a valid email address.", e)
				order = append(order, k)
			}
		}
		if len(fields) > 0 {
			abortFields(c, fields, order...)
			return
		}
	case "in_app":
		users = b.InviteeUserIDs
		if !list && b.InviteeUserID != nil {
			users = []string{*b.InviteeUserID}
		}
		if len(users) == 0 {
			abortFields(c, map[string]string{"invitee_user_ids": "Choose at least one person to invite."}, "invitee_user_ids")
			return
		}
		for _, u := range users {
			if !uuidRe.MatchString(u) {
				abortFields(c, map[string]string{"invitee_user_ids": "Choose people from the list."}, "invitee_user_ids")
				return
			}
		}
	default:
		abortFields(c, map[string]string{"channel": "Choose link, whatsapp, email or in_app."}, "channel")
		return
	}
	if len([]rune(b.Note)) > 500 {
		abortFields(c, map[string]string{"note": "Keep the note under 500 characters."}, "note")
		return
	}
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, "Community not found.")
		return
	}

	claims := auth.FromContext(c)
	var out []byte
	var community, inviter *string
	err = h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		if err := tx.QueryRow(c, `select public.create_invitations($1, $2, $3, $4::uuid[], $5, $6)::text`,
			c.Param("id"), b.Channel, emails, users, b.ExpiresAt, b.MaxUses).Scan(&out); err != nil {
			return err
		}
		if len(emails) == 0 {
			return nil
		}
		return tx.QueryRow(c, `select (select name from public.communities where id = $1),
			(select full_name from public.profiles where id = auth.uid())`, c.Param("id")).Scan(&community, &inviter)
	})
	if err != nil {
		writeErr(c, err)
		return
	}

	if len(emails) > 0 {
		var issued []issuedInvitation
		if err := json.Unmarshal(out, &issued); err == nil {
			go h.sendInviteEmails(c.Param("id"), deref(community, "a community"), deref(inviter, "A member"), b.Note, issued)
		}
	}

	if list {
		c.Data(http.StatusCreated, "application/json; charset=utf-8", out)
		return
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) == 0 {
		c.Data(http.StatusCreated, "application/json; charset=utf-8", out)
		return
	}
	c.Data(http.StatusCreated, "application/json; charset=utf-8", rows[0])
}

func deref(s *string, def string) string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return def
	}
	return *s
}

// inviteLink is the app link the invite landing opens: /communities/:id/invite/:code.
func (h *invitationHandlers) inviteLink(cid, code string) string {
	base := h.d.Config.AppLinkBase
	if base == "" {
		base = "intouch://"
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + "communities/" + cid + "/invite/" + code
}

// sendInviteEmails delivers the platform invite emails after the commit; a
// failed send marks that invitation tracking_state=failed.
func (h *invitationHandlers) sendInviteEmails(cid, community, inviter, note string, issued []issuedInvitation) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, inv := range issued {
		if inv.InviteeEmail == nil {
			continue
		}
		text := fmt.Sprintf("%s invited you to join %s on InTouch.\n\n", inviter, community)
		if n := strings.TrimSpace(note); n != "" {
			text += n + "\n\n"
		}
		text += fmt.Sprintf("Accept the invitation: %s\nInvite code: %s\n\nIf you don't have an account yet, you'll create one first and come straight back to this invitation.\n",
			h.inviteLink(cid, inv.Code), inv.Code)
		msg := mailer.Message{
			To:      *inv.InviteeEmail,
			Subject: fmt.Sprintf("%s invited you to %s on InTouch", inviter, community),
			Text:    text,
		}
		if err := h.d.Mailer.Send(ctx, msg); err != nil {
			log.Printf("invite email %s: %v", inv.ID, err)
			if h.d.DB == nil {
				continue
			}
			if err := h.d.DB.AsService(ctx, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `select public.mark_invitation_failed($1)`, inv.ID)
				return err
			}); err != nil {
				log.Printf("mark invitation %s failed: %v", inv.ID, err)
			}
		}
	}
}

// GET /communities/{id}/invitations/{code} — public landing. 200 with the
// Invitation fields plus `community` (preview) and `questions`; records
// tracking_state=opened. Every unusable code is 404 with invalidInviteMsg.
func (h *invitationHandlers) open(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, invalidInviteMsg)
		return
	}
	var out []byte
	err := h.d.DB.AsAnon(c, func(tx pgx.Tx) error {
		return tx.QueryRow(c, `select public.open_invitation($1, $2)::text`, c.Param("id"), c.Param("code")).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	var doc map[string]json.RawMessage
	var inv map[string]json.RawMessage
	if json.Unmarshal(out, &doc) != nil || json.Unmarshal(doc["invitation"], &inv) != nil {
		c.Data(http.StatusOK, "application/json; charset=utf-8", out)
		return
	}
	inv["community"] = doc["community"]
	inv["questions"] = doc["questions"]
	c.JSON(http.StatusOK, inv)
}

// POST /communities/{id}/invitations/{code}/accept — 200 membership (joined
// via=invite, or the existing one unchanged); 400 blank required answer;
// 404 unknown code; 410 expired, revoked or used up.
func (h *invitationHandlers) accept(c *gin.Context) {
	raw, err := readBody(c)
	answers, ok := parseAnswers(raw)
	if err != nil || !ok {
		apierr.Abort(c, http.StatusBadRequest, "Answers must be a list of {question_id, value}.")
		return
	}
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, invalidInviteMsg)
		return
	}
	claims := auth.FromContext(c)
	name, avatar := oauthMetadata(claims)
	var out []byte
	err = h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		// The invitee may come straight from sign-up, before GET /me.
		if _, err := tx.Exec(c, ensureProfileSQL, name, avatar); err != nil {
			return err
		}
		return tx.QueryRow(c, `select to_jsonb(m)::text from public.accept_invitation($1, $2, $3::jsonb) m`,
			c.Param("id"), c.Param("code"), string(answers)).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}

// DELETE /communities/{id}/invitations/{code} — 204; 403 caller cannot invite.
func (h *invitationHandlers) revoke(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, "Invitation not found.")
		return
	}
	(&admissionHandlers{d: h.d}).rpcNoContent(c, `select public.revoke_invitation($1, $2)`, c.Param("id"), c.Param("code"))
}
