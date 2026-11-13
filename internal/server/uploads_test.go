package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/config"
	"github.com/intouch/api/internal/storage"
)

type fakeSigner struct {
	calls []string
	err   error
}

func (f *fakeSigner) CreateSignedUploadURL(_ context.Context, bucket, path string) (*storage.SignedUpload, error) {
	f.calls = append(f.calls, bucket+"/"+path)
	if f.err != nil {
		return nil, f.err
	}
	return &storage.SignedUpload{URL: "https://x.supabase.co/storage/v1/object/upload/sign/" + bucket + "/" + path + "?token=t", Token: "t"}, nil
}

const uploader = "22222222-2222-2222-2222-222222222222"

func uploadEngine(s storage.Signer) *gin.Engine {
	gin.SetMode(gin.TestMode)
	return New(Deps{Config: config.Config{JWTSecret: secret}, Storage: s})
}

func postUpload(t *testing.T, r http.Handler, purpose, mime string, bytes int64) (int, map[string]any) {
	t.Helper()
	q := url.Values{"purpose": {purpose}, "mime": {mime}, "bytes": {strconv.FormatInt(bytes, 10)}}
	w := do(t, r, http.MethodPost, "/uploads?"+q.Encode(),
		map[string]string{"Authorization": "Bearer " + token(uploader, time.Now().Add(time.Hour))})
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func errMsg(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

const mb = 1024 * 1024

func TestUploadRejectionsNeverSign(t *testing.T) {
	f := &fakeSigner{}
	r := uploadEngine(f)
	cases := []struct {
		purpose, mime string
		bytes         int64
		status        int
		msg           string
	}{
		{"avatar", "image/jpeg", 3*mb + 1, 413, "Image too large (max 3MB)"},
		{"logo", "application/pdf", 200 * 1024, 415, "Logo must be an image"},
		{"cover", "application/pdf", 200 * 1024, 415, "Cover must be an image"},
		{"post_video", "video/mp4", 60 * mb, 413, "Video must be under 50 MB"},
		{"post_image", "image/jpeg", 6 * mb, 413, "Image must be under 5 MB"},
		{"chat", "application/pdf", 26 * mb, 413, "File must be under 25 MB"},
		{"resume", "image/jpeg", 100 * 1024, 415, "Resume must be a PDF or DOCX"},
		{"banner", "image/jpeg", 100, 400, ""},
		{"avatar", "", 100, 400, "Give the file's MIME type."},
		{"avatar", "image/jpeg", 0, 400, "File size must be a positive number of bytes."},
	}
	for _, c := range cases {
		st, body := postUpload(t, r, c.purpose, c.mime, c.bytes)
		if st != c.status || (c.msg != "" && errMsg(body) != c.msg) {
			t.Errorf("%s %s %d → %d %q, want %d %q", c.purpose, c.mime, c.bytes, st, errMsg(body), c.status, c.msg)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("signed despite rejection: %v", f.calls)
	}
}

func TestUploadIssuesSignedURL(t *testing.T) {
	f := &fakeSigner{}
	r := uploadEngine(f)
	pathRe := regexp.MustCompile(`^` + uploader + `/(\w+)/[0-9a-f]{32}\.(\w+)$`)
	cases := []struct {
		purpose, mime string
		bytes         int64
		bucket, ext   string
	}{
		{"avatar", "image/jpeg", 3 * mb, "avatars", "jpg"},
		{"logo", "image/png", 100, "community-media", "png"},
		{"post_image", "image/webp", 5 * mb, "post-media", "webp"},
		{"post_video", "video/mp4", 50 * mb, "post-media", "mp4"},
		{"chat", "audio/mp4", 2 * mb, "chat-media", "m4a"},
		{"resume", "application/pdf", mb, "resumes", "pdf"},
		{"document", "application/pdf", mb, "post-media", "pdf"},
	}
	for _, c := range cases {
		st, body := postUpload(t, r, c.purpose, c.mime, c.bytes)
		if st != http.StatusCreated {
			t.Fatalf("%s: status %d %v", c.purpose, st, body)
		}
		path, _ := body["path"].(string)
		m := pathRe.FindStringSubmatch(path)
		if body["bucket"] != c.bucket || m == nil || m[1] != c.purpose || m[2] != c.ext {
			t.Errorf("%s: bucket %v path %q", c.purpose, body["bucket"], path)
		}
		if u, _ := body["upload_url"].(string); u == "" || body["token"] != "t" || body["method"] != "PUT" {
			t.Errorf("%s: body %v", c.purpose, body)
		}
	}
	if len(f.calls) != len(cases) {
		t.Errorf("sign calls = %d", len(f.calls))
	}
}

func TestUploadAuthAndProviderFailures(t *testing.T) {
	// No bearer → 401.
	w := do(t, uploadEngine(&fakeSigner{}), http.MethodPost, "/uploads?purpose=avatar&mime=image/png&bytes=10", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no bearer = %d", w.Code)
	}
	// Storage not configured → 503 (after the checks still pass).
	gin.SetMode(gin.TestMode)
	r := New(Deps{Config: config.Config{JWTSecret: secret}})
	if st, _ := postUpload(t, r, "avatar", "image/png", 10); st != http.StatusServiceUnavailable {
		t.Errorf("no storage = %d", st)
	}
	if st, _ := postUpload(t, r, "logo", "application/pdf", 10); st != http.StatusUnsupportedMediaType {
		t.Errorf("no storage, pdf logo = %d", st)
	}
	// Storage error → 502.
	if st, _ := postUpload(t, uploadEngine(&fakeSigner{err: errors.New("down")}), "avatar", "image/png", 10); st != http.StatusBadGateway {
		t.Errorf("storage down = %d", st)
	}
}
