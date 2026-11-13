package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/profile"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Messages of the profile endpoints.
const (
	msgUsernameTaken  = "That username is taken. Try another."
	msgConfirmDelete  = "Type DELETE to confirm."
	msgDeletionReason = "Tell us why you are leaving."
	msgBadJSON        = "Request body must be a JSON object."
)

// registerMe adds OP-007..010: the caller's profile and account deletion.
func registerMe(g Groups, d Deps) {
	h := &meHandlers{d: d}
	g.User.GET("/me", h.ready, h.getMe)
	g.User.PATCH("/me", h.ready, h.updateMe)
	g.User.POST("/me/deletion", h.ready, h.requestDeletion)
	g.User.DELETE("/me/deletion", h.ready, h.cancelDeletion)
}

type meHandlers struct{ d Deps }

func (h *meHandlers) ready(c *gin.Context) {
	if h.d.DB == nil {
		apierr.Abort(c, http.StatusServiceUnavailable, "Service unavailable. Try again.")
	}
}

// writeProfile answers 200 with a profile row encoded by Postgres (to_jsonb).
func writeProfile(c *gin.Context, prof []byte) {
	c.Data(http.StatusOK, "application/json; charset=utf-8", prof)
}

// oauthMetadata returns the name and picture the identity provider (Google)
// put in the token's user_metadata claim.
func oauthMetadata(claims *auth.Claims) (name, avatar string) {
	var p struct {
		Meta map[string]any `json:"user_metadata"`
	}
	_ = json.Unmarshal(claims.Raw, &p)
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := p.Meta[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	return str("full_name", "name"), str("avatar_url", "picture")
}

const ensureProfileSQL = `select public.ensure_my_profile($1, $2)`

func (h *meHandlers) ensure(c *gin.Context, tx pgx.Tx, claims *auth.Claims) error {
	name, avatar := oauthMetadata(claims)
	_, err := tx.Exec(c, ensureProfileSQL, name, avatar)
	return err
}

func selectMe(c *gin.Context, tx pgx.Tx) ([]byte, error) {
	var prof []byte
	err := tx.QueryRow(c, `select to_jsonb(p)::text from public.profiles p where p.id = auth.uid()`).Scan(&prof)
	return prof, err
}

// GET /me — the caller's profile; the first request after a Google sign-in
// creates it (onboarding_complete false, so the client routes to Onboarding).
func (h *meHandlers) getMe(c *gin.Context) {
	claims := auth.FromContext(c)
	var prof []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		if err := h.ensure(c, tx, claims); err != nil {
			return err
		}
		var err error
		prof, err = selectMe(c, tx)
		return err
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	writeProfile(c, prof)
}

// Profile columns a client may write through PATCH /me, in display order.
var (
	meTextLimits = map[string]int{"headline": 120, "company": 100, "city": 60, "bio": 160, "avatar_url": 2048}
	meBools      = []string{"onboarding_complete", "appear_in_search", "hide_city", "activity_status", "login_alerts"}
	meOrder      = []string{"username", "full_name", "headline", "company", "city", "bio", "avatar_url",
		"skills", "interests", "onboarding_complete", "profile_visibility", "appear_in_search",
		"hide_city", "activity_status", "two_factor_enabled", "login_alerts"}
	meLabels = map[string]string{"headline": "Headline", "company": "Company", "city": "Location",
		"bio": "Bio", "avatar_url": "Photo URL"}
)

// profilePatch is a validated PATCH /me body.
type profilePatch struct {
	cols      []string
	args      []any
	twoFactor *bool
}

func (p *profilePatch) set(col string, v any) {
	p.cols = append(p.cols, col)
	p.args = append(p.args, v)
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// chips parses a skills/interests array: strings trimmed, blanks dropped,
// duplicates (case-insensitive) stored once in first-seen order.
func chips(raw json.RawMessage) ([]string, error) {
	var in []any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errors.New("Must be a list of strings.")
	}
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("Must be a list of strings.")
		}
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if utf8.RuneCountInString(s) > 60 {
			return nil, errors.New("Each item must be 60 characters or fewer.")
		}
		k := strings.ToLower(s)
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	if len(out) > 50 {
		return nil, errors.New("Pick at most 50.")
	}
	return out, nil
}

// parsePatch validates body; read-only and unknown fields are ignored
// (id, phone, is_operator, deletion_*, created_at, updated_at).
func parsePatch(body map[string]json.RawMessage) (*profilePatch, map[string]string) {
	p := &profilePatch{}
	errs := map[string]string{}
	str := func(k string) (*string, bool) {
		raw := body[k]
		if isNull(raw) {
			return nil, true
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			errs[k] = "Must be text."
			return nil, false
		}
		return &s, true
	}

	if _, ok := body["username"]; ok {
		if s, ok := str("username"); ok {
			if s == nil {
				p.set("username", nil)
			} else if err := profile.ValidateUsername(*s); err != nil {
				errs["username"] = err.Error()
			} else {
				p.set("username", *s)
			}
		}
	}
	if _, ok := body["full_name"]; ok {
		if s, ok := str("full_name"); ok {
			if s == nil || profile.ValidateFullName(*s) != nil {
				errs["full_name"] = profile.ErrFullNameLength.Error()
			} else {
				p.set("full_name", strings.TrimSpace(*s))
			}
		}
	}
	for _, k := range []string{"headline", "company", "city", "bio", "avatar_url"} {
		if _, ok := body[k]; !ok {
			continue
		}
		s, ok := str(k)
		if !ok {
			continue
		}
		if s == nil || strings.TrimSpace(*s) == "" {
			p.set(k, nil)
			continue
		}
		v := strings.TrimSpace(*s)
		switch {
		case k == "bio" && profile.ValidateBio(v) != nil:
			errs[k] = profile.ErrBioTooLong.Error()
		case utf8.RuneCountInString(v) > meTextLimits[k]:
			errs[k] = fmt.Sprintf("%s must be %d characters or fewer.", meLabels[k], meTextLimits[k])
		case k == "avatar_url" && !strings.HasPrefix(v, "https://") && !strings.HasPrefix(v, "http://"):
			errs[k] = "Photo URL must start with http:// or https://."
		default:
			p.set(k, v)
		}
	}
	for _, k := range []string{"skills", "interests"} {
		raw, ok := body[k]
		if !ok {
			continue
		}
		if isNull(raw) {
			p.set(k, "[]")
			continue
		}
		list, err := chips(raw)
		if err != nil {
			errs[k] = err.Error()
			continue
		}
		j, _ := json.Marshal(list)
		p.set(k, string(j))
	}
	boolOf := func(k string) (*bool, bool) {
		raw, ok := body[k]
		if !ok {
			return nil, false
		}
		var b bool
		if isNull(raw) || json.Unmarshal(raw, &b) != nil {
			errs[k] = "Must be true or false."
			return nil, false
		}
		return &b, true
	}
	for _, k := range meBools {
		if b, ok := boolOf(k); ok {
			p.set(k, *b)
		}
	}
	if b, ok := boolOf("two_factor_enabled"); ok {
		p.twoFactor = b
	}
	if _, ok := body["profile_visibility"]; ok {
		s, ok := str("profile_visibility")
		if ok && (s == nil || (*s != "public" && *s != "connections")) {
			errs["profile_visibility"] = "Visibility must be public or connections."
		} else if ok {
			p.set("profile_visibility", *s)
		}
	}
	return p, errs
}

// PATCH /me — onboarding, profile, skills, privacy and security settings.
// The unique index on lower(username) decides concurrent saves (409).
func (h *meHandlers) updateMe(c *gin.Context) {
	claims := auth.FromContext(c)
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil || body == nil {
		apierr.Abort(c, http.StatusBadRequest, msgBadJSON)
		return
	}
	p, errs := parsePatch(body)
	if len(errs) > 0 {
		abortFields(c, errs, meOrder...)
		return
	}

	var prof []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		if err := h.ensure(c, tx, claims); err != nil {
			return err
		}
		if len(p.cols) > 0 {
			sets := make([]string, len(p.cols))
			for i, col := range p.cols {
				// col comes from the fixed whitelist in parsePatch.
				cast := ""
				if col == "skills" || col == "interests" {
					cast = "::jsonb"
				}
				sets[i] = fmt.Sprintf("%s = $%d%s", col, i+1, cast)
			}
			q := "update public.profiles set " + strings.Join(sets, ", ") + " where id = auth.uid()"
			if _, err := tx.Exec(c, q, p.args...); err != nil {
				return err
			}
		}
		if p.twoFactor != nil {
			if _, err := tx.Exec(c, `select public.set_two_factor($1)`, *p.twoFactor); err != nil {
				return err
			}
		}
		var err error
		prof, err = selectMe(c, tx)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "profiles_username_key" {
		body := apierr.Body(http.StatusConflict, msgUsernameTaken)
		body["error"].(gin.H)["fields"] = map[string]string{"username": msgUsernameTaken}
		c.AbortWithStatusJSON(http.StatusConflict, body)
		return
	}
	if errors.As(err, &pgErr) && pgErr.Code == "PT400" {
		// 2FA without a verified phone.
		abortFields(c, map[string]string{"two_factor_enabled": pgErr.Message}, "two_factor_enabled")
		return
	}
	if err != nil {
		writeErr(c, err)
		return
	}
	writeProfile(c, prof)
}

// POST /me/deletion?reason=...&confirm=DELETE — starts the 14-day grace
// period. reason and confirm may also be sent as a JSON body.
func (h *meHandlers) requestDeletion(c *gin.Context) {
	claims := auth.FromContext(c)
	reason, confirm := c.Query("reason"), c.Query("confirm")
	if reason == "" || confirm == "" {
		var b struct {
			Reason  string `json:"reason"`
			Confirm string `json:"confirm"`
		}
		if c.Request.ContentLength != 0 {
			_ = c.ShouldBindJSON(&b)
		}
		if reason == "" {
			reason = b.Reason
		}
		if confirm == "" {
			confirm = b.Confirm
		}
	}
	errs := map[string]string{}
	if confirm != "DELETE" {
		errs["confirm"] = msgConfirmDelete
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		errs["reason"] = msgDeletionReason
	} else if utf8.RuneCountInString(reason) > 500 {
		errs["reason"] = "Reason must be 500 characters or fewer."
	}
	if len(errs) > 0 {
		abortFields(c, errs, "reason", "confirm")
		return
	}
	h.profileRPC(c, claims, `select to_jsonb(p)::text from public.request_account_deletion($1) p`, reason)
}

// DELETE /me/deletion — cancels a pending deletion (404 when none).
func (h *meHandlers) cancelDeletion(c *gin.Context) {
	h.profileRPC(c, auth.FromContext(c), `select to_jsonb(p)::text from public.cancel_account_deletion() p`)
}

func (h *meHandlers) profileRPC(c *gin.Context, claims *auth.Claims, q string, args ...any) {
	var prof []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		if err := h.ensure(c, tx, claims); err != nil {
			return err
		}
		return tx.QueryRow(c, q, args...).Scan(&prof)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	writeProfile(c, prof)
}
