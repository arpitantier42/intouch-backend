package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
)

const claimsKey = "auth.claims"

// RequireUser rejects requests without a valid bearer access token (401)
// and stores the claims for handlers (see FromContext).
func RequireUser(v *Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := bearer(c.GetHeader("Authorization"))
		if !ok {
			c.Header("WWW-Authenticate", `Bearer realm="intouch"`)
			apierr.Abort(c, http.StatusUnauthorized, "Sign in required")
			return
		}
		claims, err := v.Verify(token)
		if err != nil {
			c.Header("WWW-Authenticate", `Bearer realm="intouch", error="invalid_token"`)
			apierr.Abort(c, http.StatusUnauthorized, "Session missing or expired")
			return
		}
		c.Set(claimsKey, claims)
		c.Next()
	}
}

// OptionalUser stores the claims when a valid bearer token is present and
// lets anonymous requests through (public landing endpoints). An invalid
// token is still rejected with 401.
func OptionalUser(v *Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.Next()
			return
		}
		RequireUser(v)(c)
	}
}

// RequireAPIKey guards server-to-server routes with the X-API-Key header.
func RequireAPIKey(key string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !KeyEqual(key, c.GetHeader("X-API-Key")) {
			apierr.Abort(c, http.StatusUnauthorized, "Invalid API key")
			return
		}
		c.Next()
	}
}

// FromContext returns the verified claims, or nil for anonymous requests.
func FromContext(c *gin.Context) *Claims {
	v, ok := c.Get(claimsKey)
	if !ok {
		return nil
	}
	cl, _ := v.(*Claims)
	return cl
}

func bearer(h string) (string, bool) {
	const p = "bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	t := strings.TrimSpace(h[len(p):])
	return t, t != ""
}
