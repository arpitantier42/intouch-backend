package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/auth"
	"github.com/jackc/pgx/v5"
)

// registerSocial adds OP-059..061 (blocks) and OP-082..085 (connections).
// Rules live in the SQL RPCs (migrations ..._social_graph.sql and
// ..._blocks_connections.sql); handlers parse input and map errors.
// Connection changes reach both parties through Realtime (the connections
// table is in the supabase_realtime publication).
func registerSocial(g Groups, d Deps) {
	h := &socialHandlers{admissionHandlers{d: d}}
	ready := (&meHandlers{d: d}).ready
	g.User.POST("/me/blocks", ready, h.blockUser)
	g.User.DELETE("/me/blocks/:userId", ready, h.unblockUser)
	g.User.GET("/me/blocks", ready, h.listBlocks)
	g.User.POST("/connections", ready, h.sendConnectionRequest)
	g.User.PATCH("/connections/:id", ready, h.respondToConnection)
	g.User.DELETE("/connections/:id", ready, h.deleteConnection)
	g.User.GET("/me/connections", ready, h.listConnections)
}

type socialHandlers struct{ admissionHandlers }

// targetUser reads the other user's id from the body: the first of keys
// present, else "user_id".
func targetUser(c *gin.Context, keys ...string) string {
	raw, _ := readBody(c)
	if raw == nil {
		return ""
	}
	var b map[string]any
	if json.Unmarshal(raw, &b) != nil {
		return ""
	}
	for _, k := range append(keys, "user_id") {
		if s, ok := b[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// ensureThen runs ensure_my_profile (a first call after Google sign-in may
// precede GET /me) and then q as the caller, answering status with its JSON.
func (h *socialHandlers) ensureThen(c *gin.Context, status int, q string, args ...any) {
	claims := auth.FromContext(c)
	name, avatar := oauthMetadata(claims)
	var out []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, ensureProfileSQL, name, avatar); err != nil {
			return err
		}
		return tx.QueryRow(c, q, args...).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Data(status, "application/json; charset=utf-8", out)
}

// POST /me/blocks {"blocked_id"} — 201 Block; the pair's connection and
// follows are deleted. 400 self; 404 unknown user. Idempotent.
func (h *socialHandlers) blockUser(c *gin.Context) {
	uid := targetUser(c, "blocked_id")
	if uid == "" {
		abortFields(c, map[string]string{"blocked_id": "Choose a user to block."}, "blocked_id")
		return
	}
	h.ensureThen(c, http.StatusCreated, `select to_jsonb(b)::text from public.block_user($1) b`, uid)
}

// DELETE /me/blocks/{userId} — 204; the relationship stays Idle.
func (h *socialHandlers) unblockUser(c *gin.Context) {
	h.rpcNoContent(c, `select public.unblock_user($1)`, c.Param("userId"))
}

// GET /me/blocks — the caller's blocks, newest first, with `profile`.
func (h *socialHandlers) listBlocks(c *gin.Context) {
	h.rpcJSON(c, http.StatusOK, `select public.list_blocks()::text`)
}

// POST /connections {"addressee_id"} — 201 pending Connection; 403 block;
// 409 already pending or connected.
func (h *socialHandlers) sendConnectionRequest(c *gin.Context) {
	uid := targetUser(c, "addressee_id")
	if uid == "" {
		abortFields(c, map[string]string{"addressee_id": "Choose a user to connect with."}, "addressee_id")
		return
	}
	h.ensureThen(c, http.StatusCreated,
		`select to_jsonb(r)::text from public.send_connection_request($1) r`, uid)
}

// PATCH /connections/{id}?action=accept|decline|undo — addressee only (403).
func (h *socialHandlers) respondToConnection(c *gin.Context) {
	action := c.Query("action")
	if action == "" {
		var b struct {
			Action string `json:"action"`
		}
		if raw, _ := readBody(c); raw != nil {
			_ = json.Unmarshal(raw, &b)
		}
		action = b.Action
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if action != "accept" && action != "decline" && action != "undo" {
		abortFields(c, map[string]string{"action": "Action must be accept, decline or undo."}, "action")
		return
	}
	h.rpcJSON(c, http.StatusOK, `select to_jsonb(r)::text from public.respond_to_connection($1, $2) r`,
		c.Param("id"), action)
}

// DELETE /connections/{id} — withdraw, cancel or remove; 204, nobody notified.
func (h *socialHandlers) deleteConnection(c *gin.Context) {
	h.rpcNoContent(c, `select public.delete_connection($1)`, c.Param("id"))
}

// GET /me/connections?tab=incoming|sent|connected — {tab, counts, items}.
func (h *socialHandlers) listConnections(c *gin.Context) {
	tab := strings.ToLower(strings.TrimSpace(c.Query("tab")))
	if tab != "incoming" && tab != "sent" && tab != "connected" {
		abortFields(c, map[string]string{"tab": "Tab must be incoming, sent or connected."}, "tab")
		return
	}
	h.rpcJSON(c, http.StatusOK, `select public.list_connections($1)::text`, tab)
}
