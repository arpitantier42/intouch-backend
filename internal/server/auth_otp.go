package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/authattempt"
	"github.com/intouch/api/internal/gotrue"
	"github.com/intouch/api/internal/profile"
	"github.com/jackc/pgx/v5"
)

// Messages of the OTP endpoints.
const (
	msgSignUpRejected = "We couldn't create an account with this email. Try signing in or use another email."
	msgSendFailed     = "We couldn't send the code. Try again."
	msgInvalidCode    = "Invalid code. Try again."
	msgFullCode       = "Enter the full 6-digit code"
	msgPhone          = "Enter a 10-digit mobile number"
	msgPurpose        = "purpose must be signup, recovery, phone or two_factor"
	msgAuthDown       = "Sign-in is temporarily unavailable. Try again."
)

var sixDigits = regexp.MustCompile(`^[0-9]{6}$`)

type otpHandlers struct {
	d    Deps
	auth gotrue.Client
}

func registerAuthOTP(g Groups, d Deps) {
	h := &otpHandlers{d: d, auth: d.Auth}
	g.Public.POST("/auth/sign-up", h.ready, h.signUp)
	g.Public.POST("/auth/otp", h.ready, h.sendOTP)
	g.Public.POST("/auth/otp/verify", h.ready, h.verifyOTP)
}

// ready answers 503 when the database or Supabase Auth is not configured.
func (h *otpHandlers) ready(c *gin.Context) {
	if h.d.DB == nil || h.auth == nil {
		apierr.Abort(c, http.StatusServiceUnavailable, msgAuthDown)
	}
}

// providerErr maps an Auth failure on a send to the client error: Auth's
// own rate limit → 429, anything else (email/SMS provider down, Auth down)
// → 502 so the client offers a retry.
func providerErr(err error) error {
	if gotrue.StatusOf(err) == http.StatusTooManyRequests {
		return apierr.New(http.StatusTooManyRequests, "Please wait before requesting another code.")
	}
	return &apierr.Error{Status: http.StatusBadGateway, Message: msgSendFailed, Err: err}
}

// throttledSend logs the send (PT429 inside 30 s of the previous one) and
// calls send in the same transaction, so a failed send is rolled back and
// does not start the cooldown. The per-identifier advisory lock taken by
// auth_otp_send serialises concurrent sends.
func (h *otpHandlers) throttledSend(ctx context.Context, idHash string, send func() error) error {
	return h.d.DB.AsService(ctx, func(tx pgx.Tx) error {
		var ref string
		if err := tx.QueryRow(ctx, `select public.auth_otp_send($1)::text`, idHash).Scan(&ref); err != nil {
			return err
		}
		return send()
	})
}

// POST /auth/sign-up — validate, then have Auth email a 6-digit OTP. No
// profile exists until the code is verified.
func (h *otpHandlers) signUp(c *gin.Context) {
	var in profile.SignUp
	if err := c.ShouldBindJSON(&in); err != nil {
		apierr.Abort(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	if errs := in.Validate(); len(errs) > 0 {
		abortFields(c, errs)
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	name := strings.TrimSpace(in.FullName)
	err := h.throttledSend(c, authattempt.Hash(email), func() error {
		u, err := h.auth.SignUp(c, email, in.Password, map[string]any{"full_name": name})
		if err != nil {
			s := gotrue.StatusOf(err)
			if s == http.StatusUnprocessableEntity || s == http.StatusBadRequest {
				return apierr.New(http.StatusUnprocessableEntity, msgSignUpRejected)
			}
			return providerErr(err)
		}
		// Auth answers an already-registered email with an obfuscated user
		// that has no identities and sends nothing.
		if u == nil || u.Identities != nil && len(u.Identities) == 0 {
			return apierr.New(http.StatusUnprocessableEntity, msgSignUpRejected)
		}
		return nil
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"email": email, "code_length": 6, "resend_after_seconds": 30})
}

// otpTarget validates purpose + identifier and returns the normalised
// identifier and the Auth verify type.
func otpTarget(purpose, identifier string) (norm, verifyType string, err error) {
	switch purpose {
	case "signup", "recovery":
		e := strings.ToLower(strings.TrimSpace(identifier))
		if !profile.ValidateEmail(e) {
			return "", "", apierr.New(http.StatusBadRequest, profile.MsgEmail)
		}
		return e, purpose, nil
	case "phone", "two_factor":
		if strings.Contains(identifier, "@") {
			return "", "", apierr.New(http.StatusBadRequest, msgPhone)
		}
		p, perr := authattempt.NormalizeIdentifier(identifier)
		if perr != nil {
			return "", "", apierr.New(http.StatusBadRequest, msgPhone)
		}
		return p, "sms", nil
	}
	return "", "", apierr.New(http.StatusBadRequest, msgPurpose)
}

// POST /auth/otp — send or resend. The answer is the same whether or not an
// account exists: Auth's "no such user" refusals are swallowed.
func (h *otpHandlers) sendOTP(c *gin.Context) {
	purpose := c.Query("purpose")
	id, _, err := otpTarget(purpose, c.Query("identifier"))
	if err != nil {
		writeErr(c, err)
		return
	}
	err = h.throttledSend(c, authattempt.Hash(id), func() error {
		var err error
		switch purpose {
		case "signup":
			err = h.auth.Resend(c, id)
		case "recovery":
			err = h.auth.Recover(c, id)
		default:
			err = h.auth.SendSMS(c, id)
		}
		if err == nil {
			return nil
		}
		if s := gotrue.StatusOf(err); s >= 400 && s < 500 && s != http.StatusTooManyRequests {
			// Unknown account, already confirmed, phone sign-ups off, ...:
			// keep the send logged and answer exactly as for a real send.
			log.Printf("auth otp send (%s) refused by auth: %v", purpose, err)
			return nil
		}
		return providerErr(err)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "sent", "resend_after_seconds": 30})
}

// POST /auth/otp/verify — count the verify, check the code with Auth, then
// create the profile (first verification) and record the device.
func (h *otpHandlers) verifyOTP(c *gin.Context) {
	purpose := c.Query("purpose")
	id, typ, err := otpTarget(purpose, c.Query("identifier"))
	if err != nil {
		writeErr(c, err)
		return
	}
	if !sixDigits.MatchString(c.Query("code")) {
		apierr.Abort(c, http.StatusBadRequest, msgFullCode)
		return
	}
	idHash := authattempt.Hash(id)

	// 1. Count this verify against the latest OTP (committed even when the
	// code turns out wrong): PT401 none sent, PT410 > 10 min, PT429 > 5.
	var attempt string
	if err := h.d.DB.AsService(c, func(tx pgx.Tx) error {
		return tx.QueryRow(c, `select public.auth_otp_verify_begin($1)::text`, idHash).Scan(&attempt)
	}); err != nil {
		writeErr(c, err)
		return
	}

	// 2. Supabase Auth checks the code itself.
	sess, err := h.auth.Verify(c, typ, id, c.Query("code"))
	if err != nil {
		if s := gotrue.StatusOf(err); s >= 400 && s < 500 && s != http.StatusTooManyRequests {
			apierr.Abort(c, http.StatusUnauthorized, msgInvalidCode)
			return
		}
		if gotrue.StatusOf(err) == http.StatusTooManyRequests {
			apierr.Abort(c, http.StatusTooManyRequests, "Too many attempts. Request a new code.")
			return
		}
		writeErr(c, &apierr.Error{Status: http.StatusBadGateway, Message: msgAuthDown, Err: err})
		return
	}
	claims, err := h.d.Verifier.Verify(sess.AccessToken)
	if err != nil {
		writeErr(c, &apierr.Error{Status: http.StatusBadGateway, Message: msgAuthDown, Err: err})
		return
	}

	// 3. Mark success, create the profile if this is the first verification
	// and record the device/session.
	name, _ := sess.User.UserMetadata["full_name"].(string)
	device := c.GetHeader("X-Device-Id")
	if device == "" {
		device = c.Request.UserAgent()
	}
	var prof json.RawMessage
	err = h.d.DB.AsService(c, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, `select public.auth_attempt_succeeded($1)`, attempt); err != nil {
			return err
		}
		if err := tx.QueryRow(c, `select to_jsonb(p) from public.ensure_profile($1, $2) p`,
			claims.Subject, name).Scan(&prof); err != nil {
			return err
		}
		if claims.SessionID == "" {
			return nil
		}
		// A new-device alert only makes sense for a returning sign-in.
		alert := purpose == "phone" || purpose == "two_factor"
		_, err := tx.Exec(c, `select public.record_sign_in($1, $2, $3, $4, $5)`,
			claims.Subject, claims.SessionID, authattempt.Hash(device), c.Request.UserAgent(), alert)
		return err
	})
	if err != nil {
		writeErr(c, err)
		return
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

// writeErr writes err with apierr, logging the cause of 5xx answers.
func writeErr(c *gin.Context, err error) {
	var e *apierr.Error
	if errors.As(err, &e) && e.Status >= 500 && e.Err != nil {
		log.Printf("%s %s: %v", c.Request.Method, c.FullPath(), e.Err)
	}
	apierr.Write(c, err)
}

// abortFields answers 400 with the first message and every field error.
func abortFields(c *gin.Context, fields map[string]string) {
	var msg string
	for _, k := range []string{"full_name", "email", "password", "confirm_password"} {
		if m, ok := fields[k]; ok {
			msg = m
			break
		}
	}
	body := apierr.Body(http.StatusBadRequest, msg)
	body["error"].(gin.H)["fields"] = fields
	c.AbortWithStatusJSON(http.StatusBadRequest, body)
}
