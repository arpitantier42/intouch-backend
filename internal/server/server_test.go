package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/config"
)

const secret = "test-secret-test-secret-test-secret-123"

func do(t *testing.T, r http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHealthz(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if w := do(t, New(), http.MethodGet, "/healthz", nil); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func testEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	cfg := config.Config{JWTSecret: secret, WebhookAPIKey: "hook-key"}
	r := New(Deps{Config: cfg})
	// Probe routes standing in for endpoint tickets.
	echo := func(c *gin.Context) {
		sub := ""
		if cl := auth.FromContext(c); cl != nil {
			sub = cl.Subject
		}
		c.JSON(http.StatusOK, gin.H{"sub": sub})
	}
	r.GET("/probe/user", auth.RequireUser(auth.NewVerifier(secret)), echo)
	r.GET("/probe/public", auth.OptionalUser(auth.NewVerifier(secret)), echo)
	r.POST("/internal/probe", auth.RequireAPIKey("hook-key"), echo)
	return r
}

func token(sub string, exp time.Time) string {
	return auth.Sign(secret, map[string]any{"sub": sub, "role": "authenticated", "aud": "authenticated", "exp": exp.Unix()})
}

func TestUserRoutesRequireValidBearer(t *testing.T) {
	r := testEngine()
	good := token("11111111-1111-1111-1111-111111111111", time.Now().Add(time.Hour))
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"missing", nil, 401},
		{"not bearer", map[string]string{"Authorization": "Basic abc"}, 401},
		{"garbage", map[string]string{"Authorization": "Bearer abc.def.ghi"}, 401},
		{"expired", map[string]string{"Authorization": "Bearer " + token("x", time.Now().Add(-time.Minute))}, 401},
		{"wrong secret", map[string]string{"Authorization": "Bearer " + auth.Sign("other-secret-other-secret-other-1234", map[string]any{"sub": "x", "role": "authenticated", "aud": "authenticated", "exp": time.Now().Add(time.Hour).Unix()})}, 401},
		{"anon key token", map[string]string{"Authorization": "Bearer " + auth.Sign(secret, map[string]any{"role": "anon", "exp": time.Now().Add(time.Hour).Unix()})}, 401},
		{"valid", map[string]string{"Authorization": "Bearer " + good}, 200},
	}
	for _, tc := range cases {
		w := do(t, r, http.MethodGet, "/probe/user", tc.hdr)
		if w.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, w.Code, tc.want, w.Body)
		}
		if tc.want == 401 {
			var body struct {
				Error struct {
					Status int    `json:"status"`
					Code   string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Status != 401 || body.Error.Code != "unauthorized" {
				t.Errorf("%s: error body = %s", tc.name, w.Body)
			}
		}
	}
	if w := do(t, r, http.MethodGet, "/probe/user", map[string]string{"Authorization": "Bearer " + good}); !json.Valid(w.Body.Bytes()) || w.Body.String() != `{"sub":"11111111-1111-1111-1111-111111111111"}` {
		t.Errorf("claims not exposed: %s", w.Body)
	}
}

func TestPublicRoutesAllowAnonymousButRejectBadTokens(t *testing.T) {
	r := testEngine()
	if w := do(t, r, http.MethodGet, "/probe/public", nil); w.Code != 200 {
		t.Fatalf("anonymous: %d", w.Code)
	}
	if w := do(t, r, http.MethodGet, "/probe/public", map[string]string{"Authorization": "Bearer nope"}); w.Code != 401 {
		t.Fatalf("bad token: %d", w.Code)
	}
}

func TestInternalRoutesRequireAPIKey(t *testing.T) {
	r := testEngine()
	if w := do(t, r, http.MethodPost, "/internal/probe", nil); w.Code != 401 {
		t.Fatalf("no key: %d", w.Code)
	}
	if w := do(t, r, http.MethodPost, "/internal/probe", map[string]string{"X-API-Key": "wrong"}); w.Code != 401 {
		t.Fatalf("wrong key: %d", w.Code)
	}
	if w := do(t, r, http.MethodPost, "/internal/probe", map[string]string{"X-API-Key": "hook-key"}); w.Code != 200 {
		t.Fatalf("good key: %d", w.Code)
	}
}

func TestUnknownRouteUsesErrorBody(t *testing.T) {
	w := do(t, testEngine(), http.MethodGet, "/nope", nil)
	if w.Code != 404 || !json.Valid(w.Body.Bytes()) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
