package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/uploads"
	"github.com/jackc/pgx/v5"
)

// registerCommunities adds OP-012..015, 023, 024: create, update, overview,
// listing, soft delete and restore. Rules live in the SQL RPCs (migration
// ..._communities_api.sql); handlers parse input and map errors.
func registerCommunities(g Groups, d Deps) {
	h := &communityHandlers{admissionHandlers{d: d}}
	ready := (&meHandlers{d: d}).ready
	g.User.POST("/communities", ready, h.create)
	g.User.GET("/communities", ready, h.list)
	g.User.GET("/communities/:id", ready, h.overview)
	g.User.PATCH("/communities/:id", ready, h.update)
	g.User.DELETE("/communities/:id", ready, h.remove)
	g.User.POST("/communities/:id/restore", ready, h.restore)
}

type communityHandlers struct{ admissionHandlers }

// communityID returns the path id, answering 404 when it is not a uuid.
func communityID(c *gin.Context) (string, bool) {
	id := c.Param("id")
	if !uuidRe.MatchString(id) {
		apierr.Abort(c, http.StatusNotFound, "Community not found")
		return "", false
	}
	return id, true
}

type createCommunityBody struct {
	Name         *string `json:"name"`
	LocationType *string `json:"location_type"`
	City         *string `json:"city"`
	Visibility   *string `json:"visibility"`
	CreationKey  *string `json:"creation_key"`
}

func strOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return strings.TrimSpace(*p)
}

// POST /communities — 201 created; 200 when creation_key was already used
// by the caller (the existing community); 400 field errors.
func (h *communityHandlers) create(c *gin.Context) {
	raw, err := readBody(c)
	var b createCommunityBody
	if err != nil || raw == nil || json.Unmarshal(raw, &b) != nil {
		apierr.Abort(c, http.StatusBadRequest, "Send the community as a JSON object.")
		return
	}
	name, loc, city := strOr(b.Name, ""), strOr(b.LocationType, ""), strOr(b.City, "")
	vis := strOr(b.Visibility, "private")
	key := strOr(b.CreationKey, "")
	fields := map[string]string{}
	if name == "" {
		fields["name"] = "Enter a community name."
	} else if len([]rune(name)) > 80 {
		fields["name"] = "Community name must be 80 characters or fewer."
	}
	if loc != "place" && loc != "online" {
		fields["location_type"] = "Choose Place-based or Online only."
	} else if loc == "place" && city == "" {
		fields["city"] = "Enter a city or area."
	}
	if vis != "private" && vis != "public" {
		fields["visibility"] = "Visibility must be private or public."
	}
	if key != "" && !uuidRe.MatchString(key) {
		fields["creation_key"] = "creation_key must be a uuid."
	}
	if len(fields) > 0 {
		abortFields(c, fields, "name", "location_type", "city", "visibility", "creation_key")
		return
	}
	var keyArg any
	if key != "" {
		keyArg = key
	}
	if loc == "online" {
		city = ""
	}
	claims := auth.FromContext(c)
	var out []byte
	var existed bool
	cname := name
	err = h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		name, avatar := oauthMetadata(claims)
		if _, err := tx.Exec(c, ensureProfileSQL, name, avatar); err != nil {
			return err
		}
		// A new row's created_at is this transaction's now(); an earlier
		// community with the same key is older.
		return tx.QueryRow(c, `
			select public._community_json(r), r.created_at < now()
			from public.create_community($1, $2, coalesce($3::uuid, gen_random_uuid()), nullif($4, ''), $5) r`,
			cname, loc, keyArg, city, vis).Scan(&out, &existed)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	c.Data(status, "application/json; charset=utf-8", out)
}

// GET /communities?view=mine|trending|popular|recommended|search&q=
func (h *communityHandlers) list(c *gin.Context) {
	view := strings.TrimSpace(c.Query("view"))
	switch view {
	case "mine", "trending", "popular", "recommended", "search":
	default:
		abortFields(c, map[string]string{"view": "view must be mine, trending, popular, recommended or search."}, "view")
		return
	}
	h.rpcJSON(c, http.StatusOK, `select public.list_communities($1, $2)`, view, c.Query("q"))
}

// GET /communities/{id}?invite_code= — overview; 403 'Invitation required';
// 404 unknown or deleted.
func (h *communityHandlers) overview(c *gin.Context) {
	id, ok := communityID(c)
	if !ok {
		return
	}
	h.rpcJSON(c, http.StatusOK, `select public.community_overview($1, $2)`, id, c.Query("invite_code"))
}

// PATCH /communities/{id} — 415 when logo_url / cover_url is not an image.
func (h *communityHandlers) update(c *gin.Context) {
	id, ok := communityID(c)
	if !ok {
		return
	}
	raw, err := readBody(c)
	var patch map[string]json.RawMessage
	if err != nil || raw == nil || json.Unmarshal(raw, &patch) != nil {
		apierr.Abort(c, http.StatusBadRequest, "Send the fields to change as a JSON object.")
		return
	}
	for _, f := range []struct{ key, purpose string }{{"logo_url", "logo"}, {"cover_url", "cover"}} {
		v, ok := patch[f.key]
		if !ok {
			continue
		}
		var s *string
		if json.Unmarshal(v, &s) != nil {
			abortFields(c, map[string]string{f.key: f.key + " must be a URL."}, f.key)
			return
		}
		if s != nil && strings.TrimSpace(*s) != "" && !isImageURL(f.purpose, *s) {
			p, _ := uploads.Lookup(f.purpose)
			apierr.Abort(c, http.StatusUnsupportedMediaType, p.BadType)
			return
		}
	}
	h.rpcJSON(c, http.StatusOK, `select public.update_community($1, $2::jsonb)`, id, string(raw))
}

// isImageURL reports whether the URL's file extension is one the upload
// policy for purpose (logo, cover) accepts.
func isImageURL(purpose, s string) bool {
	p, ok := uploads.Lookup(purpose)
	if !ok {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return false
	}
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(u.Path)), ".")
	if ext == "" {
		return false
	}
	for _, e := range p.MIME {
		if strings.TrimPrefix(e, ".") == ext || (ext == "jpeg" && strings.TrimPrefix(e, ".") == "jpg") {
			return true
		}
	}
	return false
}

// DELETE /communities/{id}?confirm_name= — 202 {id, deleted_at, purge_at}.
func (h *communityHandlers) remove(c *gin.Context) {
	id, ok := communityID(c)
	if !ok {
		return
	}
	confirm, has := c.GetQuery("confirm_name")
	if !has {
		var b struct {
			ConfirmName string `json:"confirm_name"`
		}
		if raw, _ := readBody(c); raw != nil {
			_ = json.Unmarshal(raw, &b)
		}
		confirm = b.ConfirmName
	}
	h.rpcJSON(c, http.StatusAccepted, `select public.delete_community($1, $2)`, id, confirm)
}

// POST /communities/{id}/restore — 200 restored; 410 after 14 days.
func (h *communityHandlers) restore(c *gin.Context) {
	id, ok := communityID(c)
	if !ok {
		return
	}
	h.rpcJSON(c, http.StatusOK, `select public._community_json(public.restore_community($1))`, id)
}
