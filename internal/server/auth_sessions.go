package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/authattempt"
	"github.com/intouch/api/internal/db"
	"github.com/intouch/api/internal/gotrue"
	"github.com/intouch/api/internal/mailer"
	"github.com/intouch/api/internal/profile"
	"github.com/jackc/pgx/v5"
)

// Messages of the password and session endpoints.
const (
	msgBadCredentials  = "Incorrect email or password."
	msgCredentialsReq  = "Enter your email and password"
	msgCurrentPassword = "Current password is incorrect."
	msgScope           = "scope must be local or others"
)

func registerAuthSessions(g Groups, d Deps) {
	h := &otpHandlers{d: d, auth: d.Auth}
	g.Public.POST("/auth/sign-in", h.ready, h.signIn)
	g.User.POST("/auth/password", h.ready, h.setPassword)
	g.User.POST("/auth/sign-out", h.ready, h.signOut)
	g.User.GET("/me/devices", h.listDevices)
}

// sessionRevoked is the RequireUser revocation check: a token whose session
// was signed out (scope=local) or that predates a "sign out others" /
// password change of its user (unless it is the kept session) is rejected.
func sessionRevoked(d *db.DB) func(context.Context, *auth.Claims) (bool, error) {
	return func(ctx context.Context, c *auth.Claims) (bool, error) {
		var revoked bool
		var sid *string
		if c.SessionID != "" {
			sid = &c.SessionID
		}
		var iat *time.Time
		if c.IssuedAt > 0 {
			t := time.Unix(c.IssuedAt, 0)
			iat = &t
		}
		err := d.AsService(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select public.session_revoked($1::uuid, $2::uuid, $3)`,
				c.Subject, sid, iat).Scan(&revoked)
		})
		return revoked, err
	}
}

// credentials reads the sign-in email (query or JSON body) and password
// (JSON body, or the password header the contract names).
func credentials(c *gin.Context) (email, password string) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if c.Request.ContentLength != 0 {
		_ = c.ShouldBindJSON(&body)
	}
	email = c.Query("email")
	if email == "" {
		email = body.Email
	}
	password = body.Password
	if password == "" {
		password = c.GetHeader("password")
	}
	return strings.ToLower(strings.TrimSpace(email)), password
}

// checkPassword counts a password attempt for email against the lockout
// (PT423 while locked) and asks Auth whether the password is right. A wrong
// password commits the failure and returns ok=false; a correct one marks
// the attempt succeeded, which resets the failure count. An Auth outage
// rolls the attempt back so it does not count towards the lockout. The
// per-identifier advisory lock serialises concurrent attempts.
func (h *otpHandlers) checkPassword(ctx context.Context, email, password string) (sess *gotrue.Session, ok bool, err error) {
	err = h.d.DB.AsService(ctx, func(tx pgx.Tx) error {
		var attempt string
		if err := tx.QueryRow(ctx, `select public.auth_password_begin($1)::text`, authattempt.Hash(email)).Scan(&attempt); err != nil {
			return err
		}
		s, err := h.auth.SignInPassword(ctx, email, password)
		if err != nil {
			st := gotrue.StatusOf(err)
			if st == http.StatusTooManyRequests {
				return apierr.New(http.StatusTooManyRequests, "Too many attempts. Try again later.")
			}
			if st >= 400 && st < 500 {
				return nil // wrong credentials (or unknown/unconfirmed email): commit the failure
			}
			return &apierr.Error{Status: http.StatusBadGateway, Message: msgAuthDown, Err: err}
		}
		if _, err := tx.Exec(ctx, `select public.auth_attempt_succeeded($1)`, attempt); err != nil {
			return err
		}
		sess, ok = s, true
		return nil
	})
	return sess, ok, err
}

// bearerToken is the access token of a request RequireUser accepted
// ("Bearer <token>", scheme case-insensitive).
func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if len(h) < 7 {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// deviceHash is the hashed fingerprint of the calling device.
func deviceHash(c *gin.Context) string {
	device := c.GetHeader("X-Device-Id")
	if device == "" {
		device = c.Request.UserAgent()
	}
	return authattempt.Hash(device)
}

// POST /auth/sign-in — lockout check, Auth password grant, then either the
// SMS second factor (202) or the session (200 Profile + session).
func (h *otpHandlers) signIn(c *gin.Context) {
	email, password := credentials(c)
	if email == "" || password == "" {
		apierr.Abort(c, http.StatusBadRequest, msgCredentialsReq)
		return
	}
	if !profile.ValidateEmail(email) || len(password) > 72 {
		// Cannot be a valid account; same answer as a wrong password.
		apierr.Abort(c, http.StatusUnauthorized, msgBadCredentials)
		return
	}
	sess, ok, err := h.checkPassword(c, email, password)
	if err != nil {
		writeErr(c, err)
		return
	}
	if !ok {
		apierr.Abort(c, http.StatusUnauthorized, msgBadCredentials)
		return
	}
	claims, err := h.d.Verifier.Verify(sess.AccessToken)
	if err != nil {
		writeErr(c, &apierr.Error{Status: http.StatusBadGateway, Message: msgAuthDown, Err: err})
		return
	}

	name, _ := sess.User.UserMetadata["full_name"].(string)
	var prof json.RawMessage
	var twoFactor, alerts bool
	var phone *string
	var signIn struct {
		Alerted bool `json:"alerted"`
	}
	err = h.d.DB.AsService(c, func(tx pgx.Tx) error {
		if err := tx.QueryRow(c, `select to_jsonb(p) from public.ensure_profile($1, $2) p`,
			claims.Subject, name).Scan(&prof); err != nil {
			return err
		}
		if err := tx.QueryRow(c, `select two_factor_enabled, phone, login_alerts from public.sign_in_settings($1)`,
			claims.Subject).Scan(&twoFactor, &phone, &alerts); err != nil {
			return err
		}
		if twoFactor || claims.SessionID == "" {
			return nil
		}
		var res json.RawMessage
		if err := tx.QueryRow(c, `select public.record_sign_in($1, $2, $3, $4, true)`,
			claims.Subject, claims.SessionID, deviceHash(c), c.Request.UserAgent()).Scan(&res); err != nil {
			return err
		}
		return json.Unmarshal(res, &signIn)
	})
	if err != nil {
		writeErr(c, err)
		return
	}

	if twoFactor && phone != nil {
		h.requireSecondFactor(c, sess.AccessToken, claims, *phone)
		return
	}
	if signIn.Alerted {
		h.sendLoginAlert(email, c.Request.UserAgent())
	}
	var out map[string]any
	if err := json.Unmarshal(prof, &out); err != nil {
		writeErr(c, err)
		return
	}
	out["session"] = gin.H{
		"access_token":  sess.AccessToken,
		"refresh_token": sess.RefreshToken,
		"expires_in":    sess.ExpiresIn,
		"token_type":    "bearer",
	}
	c.JSON(http.StatusOK, out)
}

// requireSecondFactor withholds the password session (revoked in Auth and
// in the API), sends the SMS code and answers 202. The client completes the
// sign-in with POST /auth/otp/verify?purpose=two_factor&identifier=<phone>,
// which issues the session and records the device (with its alert).
func (h *otpHandlers) requireSecondFactor(c *gin.Context, accessToken string, claims *auth.Claims, phone string) {
	if err := h.auth.Logout(c, accessToken, "local"); err != nil {
		if s := gotrue.StatusOf(err); s == 0 || s >= 500 {
			writeErr(c, &apierr.Error{Status: http.StatusBadGateway, Message: msgAuthDown, Err: err})
			return
		}
	}
	if claims.SessionID != "" {
		if err := h.d.DB.AsService(c, func(tx pgx.Tx) error {
			_, err := tx.Exec(c, `select public.revoke_session($1, $2)`, claims.Subject, claims.SessionID)
			return err
		}); err != nil {
			writeErr(c, err)
			return
		}
	}
	ident := phone
	if n, err := authattempt.NormalizeIdentifier(phone); err == nil {
		ident = n
	}
	err := h.throttledSend(c, authattempt.Hash(ident), func() error {
		if err := h.auth.SendSMS(c, phone); err != nil {
			return providerErr(err)
		}
		return nil
	})
	// A code sent less than 30 s ago is still valid: answer as if sent.
	if st, _ := apierr.Status(err); err != nil && st != http.StatusTooManyRequests {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"two_factor_required":  true,
		"purpose":              "two_factor",
		"identifier":           ident,
		"phone_hint":           maskPhone(phone),
		"resend_after_seconds": 30,
	})
}

// maskPhone keeps the last 4 digits.
func maskPhone(p string) string {
	if len(p) <= 4 {
		return p
	}
	return strings.Repeat("•", len(p)-4) + p[len(p)-4:]
}

// sendLoginAlert emails the new-device alert in the background.
func (h *otpHandlers) sendLoginAlert(email, userAgent string) {
	m := mailer.Message{
		To:      email,
		Subject: "New sign-in to your InTouch account",
		Text: fmt.Sprintf("Your account was just signed in from a new device (%s) at %s UTC.\n"+
			"If this wasn't you, change your password and sign out of all other devices in Security settings.",
			userAgent, time.Now().UTC().Format("2 Jan 2006 15:04")),
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.d.Mailer.Send(ctx, m); err != nil {
			log.Printf("login alert email: %v", err)
		}
	}()
}

// POST /auth/password — set a new password. A normal session must prove the
// current password; a recovery session (issued by a recovery code) need
// not. Every other session is then signed out.
func (h *otpHandlers) setPassword(c *gin.Context) {
	claims := auth.FromContext(c)
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
		Password        string `json:"password"` // alias of new_password
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&in); err != nil {
			apierr.Abort(c, http.StatusBadRequest, "Invalid request body")
			return
		}
	}
	if in.NewPassword == "" {
		in.NewPassword = in.Password
	}
	if m := profile.ValidateNewPassword(in.NewPassword); m != "" {
		body := apierr.Body(http.StatusBadRequest, m)
		body["error"].(gin.H)["fields"] = map[string]string{"new_password": m}
		c.AbortWithStatusJSON(http.StatusBadRequest, body)
		return
	}
	token := bearerToken(c)

	if !claims.HasAMR("recovery") {
		email := strings.ToLower(claims.Email)
		if in.CurrentPassword == "" || email == "" {
			apierr.Abort(c, http.StatusUnauthorized, msgCurrentPassword)
			return
		}
		// The check issues a throwaway session; sign out others below
		// revokes it with every other session.
		_, ok, err := h.checkPassword(c, email, in.CurrentPassword)
		if err != nil {
			writeErr(c, err)
			return
		}
		if !ok {
			apierr.Abort(c, http.StatusUnauthorized, msgCurrentPassword)
			return
		}
	}

	if err := h.auth.UpdatePassword(c, token, in.NewPassword); err != nil {
		switch s := gotrue.StatusOf(err); {
		case s == http.StatusUnprocessableEntity || s == http.StatusBadRequest:
			apierr.Abort(c, http.StatusBadRequest, "Choose a different password.")
		case s == http.StatusUnauthorized || s == http.StatusForbidden:
			apierr.Abort(c, http.StatusUnauthorized, "Session missing or expired")
		default:
			writeErr(c, &apierr.Error{Status: http.StatusBadGateway, Message: msgAuthDown, Err: err})
		}
		return
	}
	if err := h.revoke(c, claims, token, "others"); err != nil {
		writeErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// revoke signs out (scope local: this session; others: every other one) in
// Supabase Auth, then in the API's revocation list, forgetting the devices.
// Auth first: if it is down nothing changes and the client can retry with
// the same token. A 4xx (session already gone) still revokes locally.
func (h *otpHandlers) revoke(ctx context.Context, claims *auth.Claims, token, scope string) error {
	if err := h.auth.Logout(ctx, token, scope); err != nil {
		if s := gotrue.StatusOf(err); s == 0 || s >= 500 || s == http.StatusTooManyRequests {
			return &apierr.Error{Status: http.StatusBadGateway, Message: msgAuthDown, Err: err}
		}
		log.Printf("auth logout (%s): %v", scope, err)
	}
	var sid *string
	if claims.SessionID != "" {
		sid = &claims.SessionID
	}
	return h.d.DB.AsService(ctx, func(tx pgx.Tx) error {
		if scope == "local" {
			if sid == nil {
				return nil
			}
			_, err := tx.Exec(ctx, `select public.revoke_session($1, $2)`, claims.Subject, *sid)
			return err
		}
		_, err := tx.Exec(ctx, `select count(*) from public.revoke_other_sessions($1, $2::uuid)`, claims.Subject, sid)
		return err
	})
}

// POST /auth/sign-out?scope=local|others
func (h *otpHandlers) signOut(c *gin.Context) {
	scope := c.Query("scope")
	if scope != "local" && scope != "others" {
		apierr.Abort(c, http.StatusBadRequest, msgScope)
		return
	}
	token := bearerToken(c)
	if err := h.revoke(c, auth.FromContext(c), token, scope); err != nil {
		writeErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// knownDevice is the KnownDevice body (device_hash is never returned).
type knownDevice struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	SessionID  string    `json:"session_id"`
	UserAgent  string    `json:"user_agent"`
	LastSeenAt time.Time `json:"last_seen_at"`
	CreatedAt  time.Time `json:"created_at"`
	Current    bool      `json:"current"`
}

// GET /me/devices — the caller's devices under RLS, most recent first.
func (h *otpHandlers) listDevices(c *gin.Context) {
	if h.d.DB == nil {
		apierr.Abort(c, http.StatusServiceUnavailable, msgAuthDown)
		return
	}
	claims := auth.FromContext(c)
	out := []knownDevice{}
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		rows, err := tx.Query(c, `select id::text, user_id::text, session_id::text, user_agent, last_seen_at, created_at
			from public.known_devices order by last_seen_at desc, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k knownDevice
			if err := rows.Scan(&k.ID, &k.UserID, &k.SessionID, &k.UserAgent, &k.LastSeenAt, &k.CreatedAt); err != nil {
				return err
			}
			k.Current = k.SessionID == claims.SessionID
			out = append(out, k)
		}
		return rows.Err()
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}
