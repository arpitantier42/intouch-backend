// Package storage is a minimal client for Supabase Storage: it issues signed
// upload URLs with the service-role key. Handlers depend on the Signer
// interface so tests can substitute a fake.
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SignedUploadTTL is how long Supabase Storage keeps a signed upload URL valid.
const SignedUploadTTL = 2 * time.Hour

// SignedUpload is a URL the client PUTs the file to (no bearer needed).
type SignedUpload struct {
	URL   string // absolute upload URL, including the token
	Token string // the upload token (also in URL)
}

// Signer issues signed upload URLs.
type Signer interface {
	CreateSignedUploadURL(ctx context.Context, bucket, path string) (*SignedUpload, error)
}

// Error is a non-2xx Storage response.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string { return fmt.Sprintf("storage: HTTP %d: %s", e.Status, e.Body) }

// HTTP calls the Storage REST API at BaseURL (the project's SUPABASE_URL).
type HTTP struct {
	BaseURL    string
	ServiceKey string
	Client     *http.Client
}

// New returns the HTTP signer for the project at baseURL.
func New(baseURL, serviceKey string) *HTTP {
	return &HTTP{BaseURL: strings.TrimRight(baseURL, "/"), ServiceKey: serviceKey,
		Client: &http.Client{Timeout: 10 * time.Second}}
}

// CreateSignedUploadURL calls POST /storage/v1/object/upload/sign/{bucket}/{path}.
func (h *HTTP) CreateSignedUploadURL(ctx context.Context, bucket, path string) (*SignedUpload, error) {
	endpoint := h.BaseURL + "/storage/v1/object/upload/sign/" + url.PathEscape(bucket) + "/" + escapePath(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.ServiceKey)
	req.Header.Set("apikey", h.ServiceKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return nil, &Error{Status: resp.StatusCode, Body: string(body)}
	}
	var out struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.URL == "" {
		return nil, fmt.Errorf("storage: unexpected sign response")
	}
	abs := out.URL
	if !strings.HasPrefix(abs, "http://") && !strings.HasPrefix(abs, "https://") {
		abs = h.BaseURL + "/storage/v1" + "/" + strings.TrimLeft(out.URL, "/")
	}
	tok := out.Token
	if tok == "" {
		if u, err := url.Parse(abs); err == nil {
			tok = u.Query().Get("token")
		}
	}
	return &SignedUpload{URL: abs, Token: tok}, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
