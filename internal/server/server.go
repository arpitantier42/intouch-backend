// Package server builds the HTTP router for the InTouch API.
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/config"
	"github.com/intouch/api/internal/db"
	"github.com/intouch/api/internal/gotrue"
	"github.com/intouch/api/internal/mailer"
)

// Deps are the shared dependencies handlers use. DB may be nil in unit tests.
type Deps struct {
	Config   config.Config
	DB       *db.DB
	Verifier *auth.Verifier
	// Auth is the Supabase Auth client; when nil and SUPABASE_URL is set,
	// New builds the HTTP client from the config.
	Auth gotrue.Client
	// Mailer sends transactional email; nil means mailer.Log.
	Mailer mailer.Mailer
}

// Groups are the route groups endpoint tickets register into:
//
//	Public  — no auth (sign-up, OTP, invite and event landing pages);
//	          a bearer token, if sent, must be valid.
//	User    — bearer access token required (401 otherwise).
//	Internal — X-API-Key required (webhooks, scheduler jobs).
type Groups struct {
	Public   *gin.RouterGroup
	User     *gin.RouterGroup
	Internal *gin.RouterGroup
}

// New returns the Gin engine with every route registered. With no Deps it
// builds an engine whose authenticated routes reject every token.
func New(deps ...Deps) *gin.Engine {
	var d Deps
	if len(deps) > 0 {
		d = deps[0]
	}
	if d.Verifier == nil {
		d.Verifier = auth.NewVerifier(d.Config.JWTSecret)
	}
	if d.Mailer == nil {
		d.Mailer = mailer.Log{}
	}
	if d.DB != nil && d.Verifier.Revoked == nil {
		d.Verifier.Revoked = sessionRevoked(d.DB)
	}
	if d.Auth == nil && d.Config.SupabaseURL != "" {
		d.Auth = gotrue.New(d.Config.SupabaseURL, d.Config.SupabaseAnonKey)
	}

	r := gin.New()
	r.Use(gin.Recovery())
	r.NoRoute(func(c *gin.Context) { apierr.Abort(c, http.StatusNotFound, "Not found") })
	r.HandleMethodNotAllowed = true
	r.NoMethod(func(c *gin.Context) { apierr.Abort(c, http.StatusMethodNotAllowed, "Method not allowed") })

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	g := Groups{
		Public:   r.Group("/", auth.OptionalUser(d.Verifier)),
		User:     r.Group("/", auth.RequireUser(d.Verifier)),
		Internal: r.Group("/internal", auth.RequireAPIKey(d.Config.WebhookAPIKey)),
	}
	register(g, d)
	return r
}

// register is where endpoint tickets add their routes, e.g.
// g.User.POST("/communities", h.createCommunity).
func register(g Groups, d Deps) {
	registerAuthOTP(g, d)
	registerAuthSessions(g, d)
	registerMe(g, d)
	registerAdmission(g, d)
}
