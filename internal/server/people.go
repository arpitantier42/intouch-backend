package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/push"
	"github.com/jackc/pgx/v5"
)

// registerPeople adds OP-081 (listPeople), OP-086 (getMemberProfile),
// OP-087 (requestMentorship), OP-088/089 (follow, unfollow). Rules live in
// the SQL RPCs (migrations ..._people.sql and ..._social_graph.sql).
// Follow ships enabled server-side; the app keeps it behind its own flag.
func registerPeople(g Groups, d Deps) {
	h := &peopleHandlers{socialHandlers{admissionHandlers{d: d}}}
	ready := (&meHandlers{d: d}).ready
	g.User.GET("/people", ready, h.listPeople)
	g.User.GET("/users/:id", ready, h.getMemberProfile)
	g.User.POST("/users/:id/mentorship-requests", ready, h.requestMentorship)
	g.User.PUT("/users/:id/follow", ready, h.followUser)
	g.User.DELETE("/users/:id/follow", ready, h.unfollowUser)
}

type peopleHandlers struct{ socialHandlers }

var (
	peopleModes   = map[string]bool{"connections": true, "members": true, "suggested": true, "invitable": true}
	peopleFilters = map[string]bool{"": true, "everyone": true, "same_city": true, "near_me": true, "same_company": true}
	mentorTopics  = map[string]bool{"career": true, "system_design": true, "referral": true, "resume": true, "grad_school": true}
)

// GET /people?mode&community_id&q&filter&limit — array of person cards.
func (h *peopleHandlers) listPeople(c *gin.Context) {
	mode := strings.ToLower(strings.TrimSpace(c.Query("mode")))
	filter := strings.ToLower(strings.TrimSpace(c.Query("filter")))
	fields := map[string]string{}
	if !peopleModes[mode] {
		fields["mode"] = "Mode must be connections, members, suggested or invitable."
	}
	if !peopleFilters[filter] {
		fields["filter"] = "Filter must be everyone, same_city, near_me or same_company."
	}
	var cid *string
	if s := strings.TrimSpace(c.Query("community_id")); s != "" {
		if !uuidRe.MatchString(s) {
			fields["community_id"] = "Choose a valid community."
		}
		cid = &s
	} else if mode == "members" || mode == "invitable" {
		fields["community_id"] = "Choose a community."
	}
	limit := 50
	if s := c.Query("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 100 {
			fields["limit"] = "Limit must be between 1 and 100."
		}
		limit = n
	}
	q := strings.TrimSpace(c.Query("q"))
	if len([]rune(q)) > 100 {
		fields["q"] = "Search is too long."
	}
	if len(fields) > 0 {
		abortFields(c, fields, "mode", "community_id", "filter", "q", "limit")
		return
	}
	h.rpcJSON(c, http.StatusOK, `select public.search_people($1, $2::uuid, $3, $4, $5)::text`,
		mode, cid, q, filter, limit)
}

// GET /users/{id} — privacy-aware member profile; 404 when blocked either
// way or no shared community.
func (h *peopleHandlers) getMemberProfile(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		abortNotFound(c)
		return
	}
	h.ensureThen(c, http.StatusOK, `select public.member_profile($1)::text`, c.Param("id"))
}

// POST /users/{id}/mentorship-requests {"topic","message"} — 201
// MentorshipRequest; the recipient gets a 'mentorship' notification and a
// push. 400 no/unknown topic.
func (h *peopleHandlers) requestMentorship(c *gin.Context) {
	var b struct {
		Topic   string `json:"topic"`
		Message string `json:"message"`
	}
	if raw, _ := readBody(c); raw != nil {
		_ = json.Unmarshal(raw, &b)
	}
	topic := strings.ToLower(strings.TrimSpace(b.Topic))
	switch {
	case topic == "":
		abortFields(c, map[string]string{"topic": "Choose a topic."}, "topic")
		return
	case !mentorTopics[topic]:
		abortFields(c, map[string]string{"topic": "Choose career, system_design, referral, resume or grad_school."}, "topic")
		return
	case len([]rune(b.Message)) > 1000:
		abortFields(c, map[string]string{"message": "Message must be 1000 characters or fewer."}, "message")
		return
	}
	if !uuidRe.MatchString(c.Param("id")) {
		abortNotFound(c)
		return
	}
	claims := auth.FromContext(c)
	name, avatar := oauthMetadata(claims)
	var out []byte
	var sender string
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, ensureProfileSQL, name, avatar); err != nil {
			return err
		}
		return tx.QueryRow(c,
			`select to_jsonb(r)::text, (select p.full_name from public.profiles p where p.id = auth.uid())
			 from public.request_mentorship($1, $2, $3) r`,
			c.Param("id"), topic, b.Message).Scan(&out, &sender)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	var row struct {
		ID          string `json:"id"`
		RequesterID string `json:"requester_id"`
		RecipientID string `json:"recipient_id"`
	}
	_ = json.Unmarshal(out, &row)
	if h.d.Push != nil {
		n := push.Notification{
			UserIDs: []string{row.RecipientID},
			Title:   "Mentorship request",
			Body:    sender + " asked you for mentorship.",
			Data:    map[string]string{"kind": "mentorship", "mentorship_request_id": row.ID, "requester_id": row.RequesterID},
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = h.d.Push.Send(ctx, n)
		}()
	}
	c.Data(http.StatusCreated, "application/json; charset=utf-8", out)
}

// PUT /users/{id}/follow — 200 Follow (idempotent); 403 connections-only
// profile or a block.
func (h *peopleHandlers) followUser(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		abortNotFound(c)
		return
	}
	h.ensureThen(c, http.StatusOK, `select to_jsonb(f)::text from public.follow_user($1) f`, c.Param("id"))
}

// DELETE /users/{id}/follow — 204 whether or not a follow existed (RLS lets
// the follower delete their own row only).
func (h *peopleHandlers) unfollowUser(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		c.Status(http.StatusNoContent)
		return
	}
	h.rpcNoContent(c, `delete from public.follows where follower_id = auth.uid() and followee_id = $1`, c.Param("id"))
}

func abortNotFound(c *gin.Context) {
	writeErr(c, pgx.ErrNoRows)
}
