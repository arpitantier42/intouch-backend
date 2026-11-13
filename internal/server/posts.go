package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/uploads"
	"github.com/jackc/pgx/v5"
)

// registerPosts adds OP-029..033: the community feed and post CRUD. Rules
// live in the RPCs of migration ..._posts_api.sql; handlers validate input
// shape (text length, media count/size, poll options) and map errors.
func registerPosts(g Groups, d Deps) {
	h := &postHandlers{rpc: &admissionHandlers{d: d}, d: d}
	ready := (&meHandlers{d: d}).ready
	g.User.GET("/communities/:id/posts", ready, h.listFeed)
	g.User.POST("/posts", ready, h.createPost)
	g.User.GET("/posts/:id", ready, h.getPost)
	g.User.PATCH("/posts/:id", ready, h.updatePost)
	g.User.DELETE("/posts/:id", ready, h.deletePost)
}

type postHandlers struct {
	rpc *admissionHandlers
	d   Deps
}

const (
	postMinText  = 10
	postMaxText  = 1200
	feedPageSize = 20
)

var postNotFound = "Post not found"

// sinceCutoff maps the Date posted filter to a lower bound.
func sinceCutoff(s string, now time.Time) (*time.Time, bool) {
	var d time.Duration
	switch s {
	case "", "any", "all":
		return nil, true
	case "24h":
		d = 24 * time.Hour
	case "week":
		d = 7 * 24 * time.Hour
	case "month":
		d = 30 * 24 * time.Hour
	default:
		return nil, false
	}
	t := now.Add(-d)
	return &t, true
}

// parseCursor reads "created_at,id" (RFC 3339 timestamp, uuid).
func parseCursor(s string) (*time.Time, *string, bool) {
	if s == "" {
		return nil, nil, true
	}
	at, id, ok := strings.Cut(s, ",")
	if !ok || !uuidRe.MatchString(id) {
		return nil, nil, false
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, nil, false
	}
	return &t, &id, true
}

// GET /communities/{id}/posts?chip&type&since&sort&cursor&newer_than&limit
// — 200 JSON array of posts (page of 20 by default), header X-Next-Cursor
// ("created_at,id" of the last post; absent on the last page); 403 'Members
// only'. newer_than (RFC 3339) returns posts published after it
// (pull-to-refresh).
func (h *postHandlers) listFeed(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, "Community not found")
		return
	}
	fields := map[string]string{}
	chip := strings.ToLower(c.DefaultQuery("chip", "all"))
	if chip != "all" && chip != "urgent" && chip != "announcements" {
		fields["chip"] = "chip must be all, urgent or announcements."
	}
	typ := strings.ToLower(c.DefaultQuery("type", "all"))
	switch typ {
	case "all", "jobs", "mentorship", "announcements", "events":
	default:
		fields["type"] = "type must be all, jobs, mentorship, announcements or events."
	}
	since, ok := sinceCutoff(strings.ToLower(c.Query("since")), time.Now())
	if !ok {
		fields["since"] = "since must be 24h, week or month."
	}
	sort := strings.ToLower(c.DefaultQuery("sort", "newest"))
	if sort != "newest" && sort != "oldest" {
		fields["sort"] = "sort must be newest or oldest."
	}
	curAt, curID, ok := parseCursor(c.Query("cursor"))
	if !ok {
		fields["cursor"] = "cursor must be \"created_at,id\"."
	}
	var newer *time.Time
	if s := c.Query("newer_than"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			fields["newer_than"] = "newer_than must be an RFC 3339 timestamp."
		} else {
			newer = &t
		}
	}
	limit := feedPageSize
	if s := c.Query("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 50 {
			fields["limit"] = "limit must be between 1 and 50."
		} else {
			limit = n
		}
	}
	if len(fields) > 0 {
		abortFields(c, fields, "chip", "type", "since", "sort", "cursor", "newer_than", "limit")
		return
	}

	claims := auth.FromContext(c)
	var out []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		return tx.QueryRow(c, `select public.list_feed($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			c.Param("id"), chip, typ, since, sort, curAt, curID, newer, limit).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	var page []struct {
		ID        string `json:"id"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(out, &page); err == nil && len(page) == limit && newer == nil {
		last := page[len(page)-1]
		if t, err := time.Parse(time.RFC3339Nano, last.CreatedAt); err == nil {
			c.Header("X-Next-Cursor", t.UTC().Format(time.RFC3339Nano)+","+last.ID)
		}
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}

// postInput is the createPost/updatePost body (Post schema plus the
// composer's media list and byte size).
type postInput struct {
	Kind           *string          `json:"kind"`
	Body           *string          `json:"body"`
	MediaURL       *string          `json:"media_url"`
	MediaKind      *string          `json:"media_kind"`
	MediaBytes     *int64           `json:"media_bytes"`
	MediaMIME      *string          `json:"media_mime"`
	Media          []map[string]any `json:"media"`
	IsUrgent       *bool            `json:"is_urgent"`
	IsAnnouncement *bool            `json:"is_announcement"`
	JobTag         *string          `json:"job_tag"`
	PollOptions    json.RawMessage  `json:"poll_options"`
	CommunityIDs   []string         `json:"community_ids"`
}

// checkText applies the 10–1200 rule to a trimmed body.
func checkText(s string) string {
	n := utf8.RuneCountInString(strings.TrimSpace(s))
	if n < postMinText || n > postMaxText {
		return "Write between 10 and 1,200 characters."
	}
	return ""
}

// POST /posts?community_ids=a,b — 201 Post; 400 text/media/poll/no target;
// 403 not joined or member announcement; 413 image > 5 MB / video > 50 MB.
func (h *postHandlers) createPost(c *gin.Context) {
	raw, err := readBody(c)
	var in postInput
	if err != nil || raw == nil || json.Unmarshal(raw, &in) != nil {
		apierr.Abort(c, http.StatusBadRequest, "Send the post as a JSON object.")
		return
	}

	// Media: at most one item; a single-element media list fills the fields.
	if len(in.Media) > 1 {
		abortFields(c, map[string]string{"media": "Add one photo or one video."}, "media")
		return
	}
	if len(in.Media) == 1 {
		m := in.Media[0]
		if s, ok := m["url"].(string); ok && in.MediaURL == nil {
			in.MediaURL = &s
		}
		if s, ok := m["kind"].(string); ok && in.MediaKind == nil {
			in.MediaKind = &s
		}
		if s, ok := m["mime"].(string); ok && in.MediaMIME == nil {
			in.MediaMIME = &s
		}
		if f, ok := m["bytes"].(float64); ok && in.MediaBytes == nil {
			b := int64(f)
			in.MediaBytes = &b
		}
	}

	fields := map[string]string{}
	hasMedia := in.MediaURL != nil && strings.TrimSpace(*in.MediaURL) != ""
	kind := ""
	if in.Kind != nil {
		kind = *in.Kind
	}
	if in.Body != nil && strings.TrimSpace(*in.Body) != "" {
		if msg := checkText(*in.Body); msg != "" {
			fields["body"] = msg
		}
	} else if !hasMedia {
		fields["body"] = "Write between 10 and 1,200 characters."
	}
	if hasMedia && (in.MediaKind == nil || *in.MediaKind == "") {
		fields["media_kind"] = "media_kind must be image, video or document."
	}
	var pollOpts any
	if len(in.PollOptions) > 0 && string(in.PollOptions) != "null" {
		var opts []string
		if json.Unmarshal(in.PollOptions, &opts) != nil {
			fields["poll_options"] = "Poll options must be a list of text."
		} else {
			n := 0
			for _, o := range opts {
				if strings.TrimSpace(o) != "" {
					n++
				}
			}
			if n < 2 || n != len(opts) || len(opts) > 10 {
				fields["poll_options"] = "A poll needs 2 to 10 options."
			}
			pollOpts = string(in.PollOptions)
		}
	} else if kind == "poll" {
		fields["poll_options"] = "A poll needs 2 to 10 options."
	}
	if hasMedia && pollOpts != nil {
		fields["media"] = "Add one photo or one video."
	}

	ids := in.CommunityIDs
	if q, ok := c.GetQuery("community_ids"); ok {
		ids = nil
		for _, s := range strings.Split(q, ",") {
			if s = strings.TrimSpace(s); s != "" {
				ids = append(ids, s)
			}
		}
		if ids == nil {
			ids = []string{}
		}
	}
	for _, s := range ids {
		if !uuidRe.MatchString(s) {
			fields["community_ids"] = "community_ids must be community ids."
			break
		}
	}
	if len(fields) > 0 {
		abortFields(c, fields, "body", "media", "media_kind", "poll_options", "community_ids")
		return
	}

	// Size cap (413) and type (415) for photos and videos.
	if hasMedia && in.MediaKind != nil {
		purpose := map[string]string{"image": "post_image", "video": "post_video", "document": "document"}[*in.MediaKind]
		if p, ok := uploads.Lookup(purpose); ok {
			if in.MediaBytes != nil && *in.MediaBytes > p.MaxBytes {
				apierr.Abort(c, http.StatusRequestEntityTooLarge, p.TooLarge)
				return
			}
			if in.MediaMIME != nil {
				if _, ok := p.Ext(*in.MediaMIME); !ok {
					apierr.Abort(c, http.StatusUnsupportedMediaType, p.BadType)
					return
				}
			}
		}
	}

	var targets any // nil = all joined communities
	if ids != nil {
		targets = ids
	}
	var kindArg any
	if kind != "" {
		kindArg = kind
	}
	jobTag := "general"
	if in.JobTag != nil && *in.JobTag != "" {
		jobTag = *in.JobTag
	}
	h.rpc.rpcJSON(c, http.StatusCreated,
		`select public.create_post($1::uuid[], $2, $3, $4, $5, $6, $7, $8, $9::jsonb)`,
		targets, kindArg, in.Body, in.MediaURL, in.MediaKind,
		in.IsUrgent != nil && *in.IsUrgent, in.IsAnnouncement != nil && *in.IsAnnouncement,
		jobTag, pollOpts)
}

// GET /posts/{id} — 200 Post; 404 'Post not found'.
func (h *postHandlers) getPost(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, postNotFound)
		return
	}
	h.rpc.rpcJSON(c, http.StatusOK, `select public.get_post($1)`, c.Param("id"))
}

// PATCH /posts/{id} — body, is_urgent, job_tag, pinned_to_profile; 200
// Post; 400 text; 403 not the author; 404 not found.
func (h *postHandlers) updatePost(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, postNotFound)
		return
	}
	raw, err := readBody(c)
	var patch map[string]json.RawMessage
	if err != nil || raw == nil || json.Unmarshal(raw, &patch) != nil {
		apierr.Abort(c, http.StatusBadRequest, "Send the changes as a JSON object.")
		return
	}
	out := map[string]json.RawMessage{}
	for _, k := range []string{"body", "is_urgent", "job_tag", "pinned_to_profile"} {
		if v, ok := patch[k]; ok {
			out[k] = v
		}
	}
	if v, ok := out["body"]; ok {
		var s *string
		if json.Unmarshal(v, &s) != nil {
			abortFields(c, map[string]string{"body": "body must be text."}, "body")
			return
		}
		if s != nil && strings.TrimSpace(*s) != "" {
			if msg := checkText(*s); msg != "" {
				abortFields(c, map[string]string{"body": msg}, "body")
				return
			}
		}
	}
	b, _ := json.Marshal(out)
	h.rpc.rpcJSON(c, http.StatusOK, `select public.update_post($1, $2::jsonb)`, c.Param("id"), string(b))
}

// DELETE /posts/{id} — 204 author or moderator+ of a target; 403 others;
// 404 not found.
func (h *postHandlers) deletePost(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, postNotFound)
		return
	}
	h.rpc.rpcNoContent(c, `select public.delete_post($1)`, c.Param("id"))
}
