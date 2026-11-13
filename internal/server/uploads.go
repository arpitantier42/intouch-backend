package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/storage"
	"github.com/intouch/api/internal/uploads"
)

// registerUploads adds OP-091. The rules are in internal/uploads; the
// storage buckets enforce the same caps (migration ..._storage_buckets.sql).
func registerUploads(g Groups, d Deps) {
	h := &uploadHandlers{d: d}
	g.User.POST("/uploads", h.create)
}

type uploadHandlers struct{ d Deps }

// POST /uploads?purpose=&mime=&bytes= — 201 with a signed upload URL for
// <bucket>/<user id>/<random>.<ext>. 400 unknown purpose / bad size, 415
// MIME not allowed for the purpose, 413 over its cap (checked in that order,
// before Storage is called), 503 when Storage is not configured, 502 when
// signing fails.
func (h *uploadHandlers) create(c *gin.Context) {
	purpose := strings.TrimSpace(c.Query("purpose"))
	pol, ok := uploads.Lookup(purpose)
	if !ok {
		abortFields(c, map[string]string{"purpose": "Purpose must be one of " + strings.Join(uploads.Purposes(), ", ") + "."}, "purpose")
		return
	}
	mime := uploads.NormalizeMIME(c.Query("mime"))
	if mime == "" {
		abortFields(c, map[string]string{"mime": "Give the file's MIME type."}, "mime")
		return
	}
	size, err := strconv.ParseInt(strings.TrimSpace(c.Query("bytes")), 10, 64)
	if err != nil || size <= 0 {
		abortFields(c, map[string]string{"bytes": "File size must be a positive number of bytes."}, "bytes")
		return
	}
	if status, msg := pol.Check(mime, size); status != 0 {
		apierr.Abort(c, status, msg)
		return
	}
	if h.d.Storage == nil {
		apierr.Abort(c, http.StatusServiceUnavailable, "Uploads are not available right now.")
		return
	}

	ext, _ := pol.Ext(mime)
	claims := auth.FromContext(c)
	path := claims.Subject + "/" + pol.Purpose + "/" + randomName() + "." + ext
	signed, err := h.d.Storage.CreateSignedUploadURL(c.Request.Context(), pol.Bucket, path)
	if err != nil {
		var se *storage.Error
		if errors.As(err, &se) {
			log.Printf("uploads: sign %s: storage HTTP %d", pol.Bucket, se.Status)
		} else {
			log.Printf("uploads: sign %s: %v", pol.Bucket, err)
		}
		apierr.Abort(c, http.StatusBadGateway, "Could not prepare the upload. Try again.")
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"purpose":    pol.Purpose,
		"bucket":     pol.Bucket,
		"path":       path,
		"mime":       mime,
		"bytes":      size,
		"max_bytes":  pol.MaxBytes,
		"upload_url": signed.URL,
		"token":      signed.Token,
		"method":     http.MethodPut,
		"expires_at": time.Now().Add(storage.SignedUploadTTL).UTC().Format(time.RFC3339),
	})
}

func randomName() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
