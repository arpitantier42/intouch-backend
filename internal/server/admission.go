package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/jackc/pgx/v5"
)

// registerAdmission adds OP-016..022: join requests and their review, the
// admin roster, role changes, removal, leave/withdraw and ownership transfer.
// Every rule lives in the SQL RPCs (migration ..._admission_roster.sql and
// the identity migration); handlers parse input and map errors.
func registerAdmission(g Groups, d Deps) {
	h := &admissionHandlers{d: d}
	ready := (&meHandlers{d: d}).ready
	g.User.POST("/communities/:id/join-requests", ready, h.requestToJoin)
	g.User.POST("/communities/:id/join-requests/:userId/decision", ready, h.reviewJoinRequest)
	g.User.GET("/communities/:id/members", ready, h.listMembers)
	g.User.PATCH("/communities/:id/members/:userId", ready, h.updateMemberRole)
	g.User.DELETE("/communities/:id/members/:userId", ready, h.removeMember)
	g.User.DELETE("/me/memberships/:communityId", ready, h.leaveCommunity)
	g.User.POST("/communities/:id/transfer", ready, h.transferOwnership)
}

type admissionHandlers struct{ d Deps }

// readBody returns the raw JSON body (nil when empty).
func readBody(c *gin.Context) ([]byte, error) {
	if c.Request.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil, nil
	}
	return b, nil
}

// parseAnswers accepts the join answers as a list of {question_id, value},
// a single {question_id, value} object, or {"answers": [...]}.
func parseAnswers(raw []byte) (json.RawMessage, bool) {
	if raw == nil {
		return json.RawMessage(`[]`), true
	}
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err == nil {
		return raw, true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	if a, ok := obj["answers"]; ok {
		if err := json.Unmarshal(a, &list); err != nil {
			return nil, false
		}
		return a, true
	}
	if _, ok := obj["question_id"]; ok {
		return json.RawMessage("[" + string(raw) + "]"), true
	}
	return json.RawMessage(`[]`), true
}

// rpcJSON runs q as the caller and writes the single JSON value it returns.
func (h *admissionHandlers) rpcJSON(c *gin.Context, status int, q string, args ...any) {
	claims := auth.FromContext(c)
	var out []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		return tx.QueryRow(c, q, args...).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Data(status, "application/json; charset=utf-8", out)
}

// rpcNoContent runs q as the caller and answers 204.
func (h *admissionHandlers) rpcNoContent(c *gin.Context, q string, args ...any) {
	claims := auth.FromContext(c)
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		_, err := tx.Exec(c, q, args...)
		return err
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// POST /communities/{id}/join-requests — 201 pending membership; 400 blank
// required answer; 403 private (invite only); 409 already pending or joined.
func (h *admissionHandlers) requestToJoin(c *gin.Context) {
	raw, err := readBody(c)
	answers, ok := parseAnswers(raw)
	if err != nil || !ok {
		apierr.Abort(c, http.StatusBadRequest, "Answers must be a list of {question_id, value}.")
		return
	}
	claims := auth.FromContext(c)
	name, avatar := oauthMetadata(claims)
	var out []byte
	err = h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		// A first request straight after Google sign-in may precede GET /me.
		if _, err := tx.Exec(c, ensureProfileSQL, name, avatar); err != nil {
			return err
		}
		return tx.QueryRow(c, `select to_jsonb(m)::text from public.request_join($1, $2::jsonb) m`,
			c.Param("id"), string(answers)).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Data(http.StatusCreated, "application/json; charset=utf-8", out)
}

// POST /communities/{id}/join-requests/{userId}/decision?decision=approve|reject
func (h *admissionHandlers) reviewJoinRequest(c *gin.Context) {
	decision := c.Query("decision")
	if decision == "" {
		var b struct {
			Decision string `json:"decision"`
		}
		if raw, _ := readBody(c); raw != nil {
			_ = json.Unmarshal(raw, &b)
		}
		decision = b.Decision
	}
	decision = strings.ToLower(strings.TrimSpace(decision))
	if decision != "approve" && decision != "reject" {
		abortFields(c, map[string]string{"decision": "Decision must be approve or reject."}, "decision")
		return
	}
	h.rpcJSON(c, http.StatusOK, `select to_jsonb(m)::text from public.review_join_request($1, $2, $3) m`,
		c.Param("id"), c.Param("userId"), decision)
}

// GET /communities/{id}/members?status=joined|pending — owners and admins only.
func (h *admissionHandlers) listMembers(c *gin.Context) {
	status := c.DefaultQuery("status", "joined")
	if status != "joined" && status != "pending" {
		abortFields(c, map[string]string{"status": "Status must be joined or pending."}, "status")
		return
	}
	h.rpcJSON(c, http.StatusOK, `select public.list_members($1, $2)::text`, c.Param("id"), status)
}

// PATCH /communities/{id}/members/{userId} {"role": "admin|moderator|member"}
func (h *admissionHandlers) updateMemberRole(c *gin.Context) {
	var b struct {
		Role *string `json:"role"`
	}
	raw, err := readBody(c)
	if err != nil || raw == nil || json.Unmarshal(raw, &b) != nil || b.Role == nil {
		abortFields(c, map[string]string{"role": "Choose admin, moderator or member."}, "role")
		return
	}
	role := strings.ToLower(strings.TrimSpace(*b.Role))
	if role != "admin" && role != "moderator" && role != "member" {
		abortFields(c, map[string]string{"role": "Role must be admin, moderator or member. Ownership moves only by transfer."}, "role")
		return
	}
	h.rpcJSON(c, http.StatusOK, `select to_jsonb(m)::text from public.set_member_role($1, $2, $3) m`,
		c.Param("id"), c.Param("userId"), role)
}

// DELETE /communities/{id}/members/{userId} — 204; access ends at once (RLS).
func (h *admissionHandlers) removeMember(c *gin.Context) {
	h.rpcNoContent(c, `select public.remove_member($1, $2)`, c.Param("id"), c.Param("userId"))
}

// DELETE /me/memberships/{communityId} — leave or withdraw; 409 for the Owner.
func (h *admissionHandlers) leaveCommunity(c *gin.Context) {
	h.rpcNoContent(c, `select public.leave_community($1)`, c.Param("communityId"))
}

// POST /communities/{id}/transfer?to_user_id=... — 200 with the new owner's
// membership; the previous owner becomes Admin.
func (h *admissionHandlers) transferOwnership(c *gin.Context) {
	to := c.Query("to_user_id")
	if to == "" {
		var b struct {
			ToUserID string `json:"to_user_id"`
		}
		if raw, _ := readBody(c); raw != nil {
			_ = json.Unmarshal(raw, &b)
		}
		to = b.ToUserID
	}
	if strings.TrimSpace(to) == "" {
		abortFields(c, map[string]string{"to_user_id": "Choose the new owner."}, "to_user_id")
		return
	}
	h.rpcJSON(c, http.StatusOK, `select to_jsonb(m)::text from public.transfer_ownership($1, $2) m`,
		c.Param("id"), to)
}
