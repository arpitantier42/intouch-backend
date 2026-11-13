// Package uploads holds the per-purpose upload rules (bucket, size cap,
// allowed MIME types) that POST /uploads checks before a signed upload URL
// is issued. The storage buckets enforce the same caps and types (migration
// ..._storage_buckets.sql); keep the two in step.
package uploads

import (
	"fmt"
	"sort"
	"strings"
)

// MB is one mebibyte; caps follow the chat schema (25 MB = 26214400 bytes).
const MB int64 = 1024 * 1024

// Policy is the rule set of one upload purpose.
type Policy struct {
	Purpose  string
	Bucket   string
	MaxBytes int64
	// MIME maps each allowed (normalised) type to the file extension used
	// for the object name.
	MIME map[string]string
	// TooLarge is the 413 message; BadType the 415 message.
	TooLarge string
	BadType  string
}

var (
	imageTypes = map[string]string{
		"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp",
		"image/heic": "heic", "image/heif": "heif", "image/gif": "gif",
	}
	videoTypes = map[string]string{
		"video/mp4": "mp4", "video/quicktime": "mov", "video/webm": "webm", "video/3gpp": "3gp",
	}
	audioTypes = map[string]string{
		"audio/mp4": "m4a", "audio/x-m4a": "m4a", "audio/aac": "aac", "audio/mpeg": "mp3",
		"audio/ogg": "ogg", "audio/webm": "weba", "audio/3gpp": "3gp", "audio/wav": "wav",
		"audio/x-wav": "wav", "audio/amr": "amr",
	}
	resumeTypes = map[string]string{
		"application/pdf": "pdf",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
	}
	documentTypes = merge(resumeTypes, map[string]string{
		"application/msword":       "doc",
		"application/vnd.ms-excel": "xls",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         "xlsx",
		"application/vnd.ms-powerpoint":                                             "ppt",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": "pptx",
		"text/plain": "txt",
		"text/csv":   "csv",
	})
	// Chat attachments: camera/gallery (image, video), documents, voice
	// notes and shared contacts (vCard). Locations are not uploads.
	chatTypes = merge(imageTypes, videoTypes, audioTypes, documentTypes, map[string]string{
		"text/vcard": "vcf", "text/x-vcard": "vcf",
	})
)

// policies by purpose. Logo and cover are images (a PDF logo is 415).
var policies = map[string]Policy{
	"avatar": {Bucket: "avatars", MaxBytes: 3 * MB, MIME: without(imageTypes, "image/gif"),
		TooLarge: "Image too large (max 3MB)", BadType: "Profile photo must be a JPEG, PNG, WebP or HEIC image"},
	"logo": {Bucket: "community-media", MaxBytes: 5 * MB, MIME: imageTypes,
		TooLarge: "Logo must be under 5 MB", BadType: "Logo must be an image"},
	"cover": {Bucket: "community-media", MaxBytes: 5 * MB, MIME: imageTypes,
		TooLarge: "Cover must be under 5 MB", BadType: "Cover must be an image"},
	"post_image": {Bucket: "post-media", MaxBytes: 5 * MB, MIME: imageTypes,
		TooLarge: "Image must be under 5 MB", BadType: "Choose an image file"},
	"post_video": {Bucket: "post-media", MaxBytes: 50 * MB, MIME: videoTypes,
		TooLarge: "Video must be under 50 MB", BadType: "Choose a video file"},
	"document": {Bucket: "post-media", MaxBytes: 25 * MB, MIME: documentTypes,
		TooLarge: "Document must be under 25 MB", BadType: "This document type is not supported"},
	"chat": {Bucket: "chat-media", MaxBytes: 25 * MB, MIME: chatTypes,
		TooLarge: "File must be under 25 MB", BadType: "This file type cannot be sent"},
	"resume": {Bucket: "resumes", MaxBytes: 10 * MB, MIME: resumeTypes,
		TooLarge: "Resume must be under 10 MB", BadType: "Resume must be a PDF or DOCX"},
}

func init() {
	for k, p := range policies {
		p.Purpose = k
		policies[k] = p
	}
}

// Lookup returns the policy of purpose (case-insensitive).
func Lookup(purpose string) (Policy, bool) {
	p, ok := policies[strings.ToLower(strings.TrimSpace(purpose))]
	return p, ok
}

// Purposes lists the known purposes, sorted.
func Purposes() []string {
	out := make([]string, 0, len(policies))
	for k := range policies {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Buckets maps each bucket to its largest cap and the union of its types —
// what the bucket itself must allow.
func Buckets() map[string]Policy {
	out := map[string]Policy{}
	for _, p := range policies {
		b, ok := out[p.Bucket]
		if !ok {
			b = Policy{Bucket: p.Bucket, MIME: map[string]string{}}
		}
		if p.MaxBytes > b.MaxBytes {
			b.MaxBytes = p.MaxBytes
		}
		for m, e := range p.MIME {
			b.MIME[m] = e
		}
		out[p.Bucket] = b
	}
	return out
}

// NormalizeMIME lower-cases mime, drops parameters (";charset=...") and maps
// common aliases (image/jpg, image/pjpeg).
func NormalizeMIME(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch m {
	case "image/jpg", "image/pjpeg":
		return "image/jpeg"
	}
	return m
}

// Ext returns the object extension for mime and whether the type is allowed.
func (p Policy) Ext(mime string) (string, bool) {
	e, ok := p.MIME[NormalizeMIME(mime)]
	return e, ok
}

// Check returns the HTTP status that rejects (mime, bytes) — 415 for a
// type the purpose does not allow, 413 over the cap, 400 for a non-positive
// size — with its message, or 0 when the upload is allowed. The type is
// checked first so a PDF logo is 415 whatever its size.
func (p Policy) Check(mime string, bytes int64) (int, string) {
	if _, ok := p.Ext(mime); !ok {
		return 415, p.BadType
	}
	if bytes <= 0 {
		return 400, "File size must be a positive number of bytes"
	}
	if bytes > p.MaxBytes {
		return 413, p.TooLarge
	}
	return 0, ""
}

func (p Policy) String() string {
	return fmt.Sprintf("%s → %s (≤ %d bytes)", p.Purpose, p.Bucket, p.MaxBytes)
}

func merge(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func without(m map[string]string, keys ...string) map[string]string {
	out := merge(m)
	for _, k := range keys {
		delete(out, k)
	}
	return out
}
