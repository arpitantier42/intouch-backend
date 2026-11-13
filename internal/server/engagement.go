package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
)

// registerEngagement adds OP-034..043: likes, comments, poll votes,
// bookmarks and content filters. Rules live in the RPCs of migration
// ..._engagement_api.sql; handlers check input shape and map errors.
func registerEngagement(g Groups, d Deps) {
	h := &engagementHandlers{rpc: &admissionHandlers{d: d}}
	ready := (&meHandlers{d: d}).ready
	g.User.PUT("/likes", ready, h.like)
	g.User.DELETE("/likes/:id", ready, h.unlike)
	g.User.GET("/posts/:id/comments", ready, h.listComments)
	g.User.POST("/posts/:id/comments", ready, h.createComment)
	g.User.PUT("/posts/:id/vote", ready, h.votePoll)
	g.User.PUT("/bookmarks", ready, h.saveBookmark)
	g.User.DELETE("/bookmarks/:id", ready, h.removeBookmark)
	g.User.GET("/me/bookmarks", ready, h.listBookmarks)
	g.User.POST("/me/content-filters", ready, h.addContentFilter)
	g.User.DELETE("/me/content-filters/:id", ready, h.removeContentFilter)
}

type engagementHandlers struct{ rpc *admissionHandlers }

const commentMaxText = 1000

// bodyObject decodes the JSON object body; false (and 400 written) otherwise.
func bodyObject(c *gin.Context, v any) bool {
	raw, err := readBody(c)
	if err != nil || raw == nil || json.Unmarshal(raw, v) != nil {
		apierr.Abort(c, http.StatusBadRequest, "Send a JSON object.")
		return false
	}
	return true
}

// optUUID returns nil for an absent/empty id, or the id; ok=false when malformed.
func optUUID(s *string) (any, bool) {
	if s == nil || *s == "" {
		return nil, true
	}
	if !uuidRe.MatchString(*s) {
		return nil, false
	}
	return *s, true
}

// exactlyOne parses two optional uuid fields of which exactly one is set.
func exactlyOne(c *gin.Context, aName string, a *string, bName string, b *string) (any, any, bool) {
	av, okA := optUUID(a)
	bv, okB := optUUID(b)
	switch {
	case !okA:
		abortFields(c, map[string]string{aName: aName + " must be a UUID."}, aName)
	case !okB:
		abortFields(c, map[string]string{bName: bName + " must be a UUID."}, bName)
	case (av == nil) == (bv == nil):
		apierr.Abort(c, http.StatusBadRequest, "Send exactly one of "+aName+" or "+bName+".")
	default:
		return av, bv, true
	}
	return nil, nil, false
}

// pathID aborts with 404 msg unless :id is a UUID.
func pathID(c *gin.Context, msg string) (string, bool) {
	id := c.Param("id")
	if !uuidRe.MatchString(id) {
		apierr.Abort(c, http.StatusNotFound, msg)
		return "", false
	}
	return id, true
}

// PUT /likes {post_id | comment_id} — 200 Like (+ like_count), repeat = no-op;
// 404 target not visible.
func (h *engagementHandlers) like(c *gin.Context) {
	var in struct {
		PostID    *string `json:"post_id"`
		CommentID *string `json:"comment_id"`
	}
	if !bodyObject(c, &in) {
		return
	}
	p, cm, ok := exactlyOne(c, "post_id", in.PostID, "comment_id", in.CommentID)
	if !ok {
		return
	}
	h.rpc.rpcJSON(c, http.StatusOK, `select public.like_target($1, $2)`, p, cm)
}

// DELETE /likes/{id} — id is the like id, or the liked post/comment id; 204
// always (removing a like that is not there is a no-op).
func (h *engagementHandlers) unlike(c *gin.Context) {
	id, ok := pathID(c, "Like not found")
	if !ok {
		return
	}
	h.rpc.rpcNoContent(c, `select public.unlike($1)`, id)
}

// GET /posts/{id}/comments?sort=relevant|recent — 200 array of top-level
// comments with nested `replies`; [] → 'Be the first to comment'.
func (h *engagementHandlers) listComments(c *gin.Context) {
	id, ok := pathID(c, postNotFound)
	if !ok {
		return
	}
	sort := c.DefaultQuery("sort", "relevant")
	if sort != "relevant" && sort != "recent" {
		abortFields(c, map[string]string{"sort": "sort must be relevant or recent."}, "sort")
		return
	}
	h.rpc.rpcJSON(c, http.StatusOK, `select public.list_comments($1, $2)`, id, sort)
}

// POST /posts/{id}/comments {body, parent_id?} — 201 Comment; 400 blank or
// > 1000 characters or parent on another post; 404 post not visible.
func (h *engagementHandlers) createComment(c *gin.Context) {
	id, ok := pathID(c, postNotFound)
	if !ok {
		return
	}
	var in struct {
		Body     *string `json:"body"`
		ParentID *string `json:"parent_id"`
	}
	if !bodyObject(c, &in) {
		return
	}
	if in.Body == nil || strings.TrimSpace(*in.Body) == "" {
		abortFields(c, map[string]string{"body": "Write a comment first."}, "body")
		return
	}
	body := strings.TrimSpace(*in.Body)
	if utf8.RuneCountInString(body) > commentMaxText {
		abortFields(c, map[string]string{"body": "Comments can be up to 1000 characters."}, "body")
		return
	}
	parent, ok := optUUID(in.ParentID)
	if !ok {
		abortFields(c, map[string]string{"parent_id": "parent_id must be a UUID."}, "parent_id")
		return
	}
	h.rpc.rpcJSON(c, http.StatusCreated, `select public.create_comment($1, $2, $3)`, id, parent, body)
}

// PUT /posts/{id}/vote {option_index} — 200 PollVote (+ my_vote,
// poll_results); a second vote replaces the first; 400 out of range or not
// a poll; 404 post not visible.
func (h *engagementHandlers) votePoll(c *gin.Context) {
	id, ok := pathID(c, postNotFound)
	if !ok {
		return
	}
	var in struct {
		OptionIndex *int `json:"option_index"`
	}
	if !bodyObject(c, &in) {
		return
	}
	if in.OptionIndex == nil || *in.OptionIndex < 0 {
		abortFields(c, map[string]string{"option_index": "Choose one of the poll options."}, "option_index")
		return
	}
	h.rpc.rpcJSON(c, http.StatusOK, `select public.vote_poll($1, $2)`, id, *in.OptionIndex)
}

// PUT /bookmarks {post_id | job_id} — 200 Bookmark (idempotent); 404 not visible.
func (h *engagementHandlers) saveBookmark(c *gin.Context) {
	var in struct {
		PostID *string `json:"post_id"`
		JobID  *string `json:"job_id"`
	}
	if !bodyObject(c, &in) {
		return
	}
	p, j, ok := exactlyOne(c, "post_id", in.PostID, "job_id", in.JobID)
	if !ok {
		return
	}
	h.rpc.rpcJSON(c, http.StatusOK, `select public.save_bookmark($1, $2)`, p, j)
}

// DELETE /bookmarks/{id} — id is the bookmark id, or the saved post/job id; 204.
func (h *engagementHandlers) removeBookmark(c *gin.Context) {
	id, ok := pathID(c, "Bookmark not found")
	if !ok {
		return
	}
	h.rpc.rpcNoContent(c, `select public.remove_bookmark($1)`, id)
}

// GET /me/bookmarks — 200 array of bookmarks with `post` or `job`; deleted
// (or no longer visible) targets are left out.
func (h *engagementHandlers) listBookmarks(c *gin.Context) {
	h.rpc.rpcJSON(c, http.StatusOK, `select public.list_bookmarks()`)
}

// POST /me/content-filters {kind: hide_post|mute_author, post_id |
// muted_user_id} — 201 ContentFilter (repeat returns the existing row);
// 400 shape / self-mute; 404 post or user not found.
func (h *engagementHandlers) addContentFilter(c *gin.Context) {
	var in struct {
		Kind        string  `json:"kind"`
		PostID      *string `json:"post_id"`
		MutedUserID *string `json:"muted_user_id"`
	}
	if !bodyObject(c, &in) {
		return
	}
	if in.Kind != "hide_post" && in.Kind != "mute_author" {
		abortFields(c, map[string]string{"kind": "kind must be hide_post or mute_author."}, "kind")
		return
	}
	p, m, ok := exactlyOne(c, "post_id", in.PostID, "muted_user_id", in.MutedUserID)
	if !ok {
		return
	}
	h.rpc.rpcJSON(c, http.StatusCreated, `select public.add_content_filter($1, $2, $3)`, in.Kind, p, m)
}

// DELETE /me/content-filters/{id} — Undo; id is the filter id (or the hidden
// post / muted user id); 204.
func (h *engagementHandlers) removeContentFilter(c *gin.Context) {
	id, ok := pathID(c, "Filter not found")
	if !ok {
		return
	}
	h.rpc.rpcNoContent(c, `select public.remove_content_filter($1)`, id)
}
