package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// registerThreads adds OP-046..049 and OP-055..057: the chat list, DMs,
// groups, adding people, per-participant settings, thread info and call
// logging. Rules live in the SQL RPCs (migration ..._chat_threads.sql).
func registerThreads(g Groups, d Deps) {
	h := &admissionHandlers{d: d}
	t := &threadHandlers{h: h}
	ready := (&meHandlers{d: d}).ready
	g.User.GET("/communities/:id/threads", ready, t.listThreads)
	g.User.POST("/communities/:id/threads/direct", ready, t.openDirect)
	g.User.POST("/communities/:id/threads/groups", ready, t.createGroup)
	g.User.POST("/threads/:id/participants", ready, t.addParticipants)
	g.User.PATCH("/threads/:id/me", ready, t.updateSettings)
	g.User.GET("/threads/:id", ready, t.getThread)
	g.User.POST("/threads/:id/calls", ready, t.logCall)
}

type threadHandlers struct{ h *admissionHandlers }

// splitIDs splits a comma list and drops blanks.
func splitIDs(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// GET /communities/{id}/threads?filter=all|unread|groups|calls&q=...
func (t *threadHandlers) listThreads(c *gin.Context) {
	filter := strings.ToLower(strings.TrimSpace(c.DefaultQuery("filter", "all")))
	switch filter {
	case "", "all", "unread", "groups", "calls":
	default:
		abortFields(c, map[string]string{"filter": "Filter must be all, unread, groups or calls."}, "filter")
		return
	}
	t.h.rpcJSON(c, http.StatusOK, `select public.list_threads($1, $2, $3)::text`,
		c.Param("id"), filter, c.Query("q"))
}

// POST /communities/{id}/threads/direct?user_id=... — 200 existing or new DM.
func (t *threadHandlers) openDirect(c *gin.Context) {
	uid := c.Query("user_id")
	if uid == "" {
		var b struct {
			UserID string `json:"user_id"`
		}
		if raw, _ := readBody(c); raw != nil {
			_ = json.Unmarshal(raw, &b)
		}
		uid = b.UserID
	}
	if strings.TrimSpace(uid) == "" {
		abortFields(c, map[string]string{"user_id": "Choose someone to message."}, "user_id")
		return
	}
	t.h.rpcJSON(c, http.StatusOK, `select to_jsonb(t)::text from public.open_dm($1, $2) t`,
		c.Param("id"), strings.TrimSpace(uid))
}

// POST /communities/{id}/threads/groups?member_ids=a,b {"name","description"}
// — member_ids may also come in the body as an array.
func (t *threadHandlers) createGroup(c *gin.Context) {
	var b struct {
		Name        *string  `json:"name"`
		Description *string  `json:"description"`
		MemberIDs   []string `json:"member_ids"`
	}
	raw, err := readBody(c)
	if err != nil || (raw != nil && json.Unmarshal(raw, &b) != nil) {
		abortFields(c, map[string]string{"name": "Send the group as JSON."}, "name")
		return
	}
	ids := splitIDs(c.Query("member_ids"))
	for _, id := range b.MemberIDs {
		ids = append(ids, splitIDs(id)...)
	}
	fields := map[string]string{}
	if b.Name == nil || strings.TrimSpace(*b.Name) == "" {
		fields["name"] = "Enter a group name."
	} else if len([]rune(strings.TrimSpace(*b.Name))) > 80 {
		fields["name"] = "Group name must be at most 80 characters."
	}
	if b.Description != nil && len([]rune(strings.TrimSpace(*b.Description))) > 300 {
		fields["description"] = "Description must be at most 300 characters."
	}
	if len(ids) == 0 {
		fields["member_ids"] = "Select at least one member."
	}
	if len(fields) > 0 {
		abortFields(c, fields, "name", "description", "member_ids")
		return
	}
	var desc any
	if b.Description != nil {
		desc = *b.Description
	}
	t.h.rpcJSON(c, http.StatusCreated,
		`select to_jsonb(t)::text from public.create_group($1, $2, $3, $4::uuid[]) t`,
		c.Param("id"), strings.TrimSpace(*b.Name), desc, ids)
}

// POST /threads/{id}/participants — body {"user_id"}, {"user_ids":[...]} or
// a list of either. Answers the added participant, or a list for lists.
func (t *threadHandlers) addParticipants(c *gin.Context) {
	raw, err := readBody(c)
	var ids []string
	single := true
	if err == nil && raw != nil {
		var one struct {
			UserID  string   `json:"user_id"`
			UserIDs []string `json:"user_ids"`
		}
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil {
			single = false
			for _, item := range list {
				var s string
				if json.Unmarshal(item, &s) == nil {
					ids = append(ids, s)
				} else if json.Unmarshal(item, &one) == nil && one.UserID != "" {
					ids = append(ids, one.UserID)
				}
			}
		} else if json.Unmarshal(raw, &one) == nil {
			if one.UserID != "" {
				ids = append(ids, one.UserID)
			}
			if len(one.UserIDs) > 0 {
				single = false
				ids = append(ids, one.UserIDs...)
			}
		}
	}
	ids = splitIDs(strings.Join(ids, ","))
	if len(ids) == 0 {
		abortFields(c, map[string]string{"user_id": "Choose who to add."}, "user_id")
		return
	}
	q := `select coalesce(jsonb_agg(to_jsonb(p)), '[]')::text from public.add_participants($1, $2::uuid[]) p`
	if single {
		q = `select to_jsonb(p)::text from public.add_participants($1, $2::uuid[]) p limit 1`
	}
	t.h.rpcJSON(c, http.StatusCreated, q, c.Param("id"), ids)
}

// settingKeys are the participant fields a client may change.
var settingKeys = []string{"muted_until", "mute", "alert_mentions", "alert_replies",
	"alert_reactions", "sound", "action"}

// PATCH /threads/{id}/me?disappearing_seconds=N — the caller's own settings.
// Body: muted_until, mute (off|1h|8h|1w|always), alert_*, sound,
// action (clear|delete|exit), disappearing_seconds. Other keys are ignored.
func (t *threadHandlers) updateSettings(c *gin.Context) {
	raw, err := readBody(c)
	body := map[string]json.RawMessage{}
	if err != nil || (raw != nil && json.Unmarshal(raw, &body) != nil) {
		abortFields(c, map[string]string{"settings": "Send the settings as a JSON object."}, "settings")
		return
	}
	fields := map[string]string{}
	settings := map[string]any{}
	for _, k := range settingKeys {
		v, ok := body[k]
		if !ok {
			continue
		}
		var val any
		_ = json.Unmarshal(v, &val)
		switch k {
		case "muted_until":
			if s, ok := val.(string); ok {
				if _, err := time.Parse(time.RFC3339, s); err != nil {
					fields[k] = "muted_until must be an RFC 3339 time or null."
					continue
				}
			} else if val != nil {
				fields[k] = "muted_until must be an RFC 3339 time or null."
				continue
			}
		case "alert_mentions", "alert_replies", "alert_reactions", "sound":
			if _, ok := val.(bool); !ok {
				fields[k] = k + " must be true or false."
				continue
			}
		case "mute", "action":
			if _, ok := val.(string); !ok {
				fields[k] = k + " must be a string."
				continue
			}
		}
		settings[k] = val
	}

	setDis := false
	var dis any
	if q, ok := c.GetQuery("disappearing_seconds"); ok {
		setDis = true
		if q = strings.TrimSpace(q); q != "" && q != "off" && q != "null" {
			n, err := strconv.Atoi(q)
			if err != nil {
				fields["disappearing_seconds"] = "Disappearing messages must be off, 24 hours, 7 days or 90 days."
			} else {
				dis = n
			}
		}
	} else if v, ok := body["disappearing_seconds"]; ok {
		setDis = true
		var n *int
		if json.Unmarshal(v, &n) != nil {
			fields["disappearing_seconds"] = "Disappearing messages must be off, 24 hours, 7 days or 90 days."
		} else if n != nil {
			dis = *n
		}
	}
	if len(fields) > 0 {
		abortFields(c, fields, append(settingKeys, "disappearing_seconds", "settings")...)
		return
	}
	js, _ := json.Marshal(settings)
	t.h.rpcJSON(c, http.StatusOK,
		`select to_jsonb(p)::text from public.update_thread_settings($1, $2::jsonb, $3, $4::int) p`,
		c.Param("id"), string(js), setDis, dis)
}

// GET /threads/{id} — info page; 403 unless an active participant.
func (t *threadHandlers) getThread(c *gin.Context) {
	t.h.rpcJSON(c, http.StatusOK, `select public.get_thread($1)::text`, c.Param("id"))
}

// POST /threads/{id}/calls {"media","outcome","duration_seconds","started_at"}
func (t *threadHandlers) logCall(c *gin.Context) {
	var b struct {
		Media     string     `json:"media"`
		Outcome   string     `json:"outcome"`
		Duration  *int       `json:"duration_seconds"`
		StartedAt *time.Time `json:"started_at"`
	}
	raw, err := readBody(c)
	if err != nil || raw == nil || json.Unmarshal(raw, &b) != nil {
		abortFields(c, map[string]string{"media": "Send the call as JSON with media and outcome."}, "media")
		return
	}
	b.Media = strings.ToLower(strings.TrimSpace(b.Media))
	b.Outcome = strings.ToLower(strings.TrimSpace(b.Outcome))
	fields := map[string]string{}
	if b.Media != "audio" && b.Media != "video" {
		fields["media"] = "Media must be audio or video."
	}
	switch b.Outcome {
	case "completed", "missed", "declined", "cancelled":
	default:
		fields["outcome"] = "Outcome must be completed, missed, declined or cancelled."
	}
	if b.Duration != nil && (*b.Duration < 0 || (b.Outcome != "completed" && fields["outcome"] == "")) {
		fields["duration_seconds"] = "Only completed calls have a non-negative duration."
	}
	if len(fields) > 0 {
		abortFields(c, fields, "media", "outcome", "duration_seconds")
		return
	}
	t.h.rpcJSON(c, http.StatusCreated,
		`select to_jsonb(l)::text from public.log_call($1, $2, $3, $4, $5) l`,
		c.Param("id"), b.Media, b.Outcome, b.Duration, b.StartedAt)
}
