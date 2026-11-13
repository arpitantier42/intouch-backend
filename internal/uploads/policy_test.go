package uploads

import "testing"

func TestCheck(t *testing.T) {
	cases := []struct {
		purpose, mime string
		bytes         int64
		want          int
		msg           string
	}{
		{"avatar", "image/jpeg", 3 * MB, 0, ""},
		{"avatar", "image/jpeg", 3*MB + 1, 413, "Image too large (max 3MB)"},
		{"logo", "application/pdf", 1000, 415, "Logo must be an image"},
		{"cover", "application/pdf", 100 * MB, 415, "Cover must be an image"},
		{"logo", "image/png", 1000, 0, ""},
		{"post_image", "image/png", 6 * MB, 413, "Image must be under 5 MB"},
		{"post_image", "image/png", 5 * MB, 0, ""},
		{"post_video", "video/mp4", 60 * MB, 413, "Video must be under 50 MB"},
		{"post_video", "video/mp4", 50 * MB, 0, ""},
		{"post_video", "image/png", 1, 415, "Choose a video file"},
		{"chat", "application/pdf", 26 * MB, 413, "File must be under 25 MB"},
		{"chat", "audio/mp4", 2 * MB, 0, ""},
		{"chat", "text/vcard", 200, 0, ""},
		{"chat", "image/jpeg", 25 * MB, 0, ""},
		{"resume", "image/jpeg", 1000, 415, "Resume must be a PDF or DOCX"},
		{"resume", "application/pdf", 1000, 0, ""},
		{"resume", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", 1000, 0, ""},
		{"document", "application/pdf", 1000, 0, ""},
		{"avatar", "image/svg+xml", 10, 415, "Profile photo must be a JPEG, PNG, WebP or HEIC image"},
		{"avatar", "image/jpeg", 0, 400, "File size must be a positive number of bytes"},
		{"avatar", "IMAGE/JPG; charset=binary", 1000, 0, ""},
	}
	for _, c := range cases {
		p, ok := Lookup(c.purpose)
		if !ok {
			t.Fatalf("unknown purpose %s", c.purpose)
		}
		got, msg := p.Check(c.mime, c.bytes)
		if got != c.want || msg != c.msg {
			t.Errorf("%s %s %d = %d %q, want %d %q", c.purpose, c.mime, c.bytes, got, msg, c.want, c.msg)
		}
	}
}

func TestLookupAndBuckets(t *testing.T) {
	if _, ok := Lookup("banner"); ok {
		t.Error("unknown purpose accepted")
	}
	if p, ok := Lookup(" Avatar "); !ok || p.Purpose != "avatar" || p.Bucket != "avatars" {
		t.Errorf("Lookup(Avatar) = %+v %v", p, ok)
	}
	b := Buckets()
	if b["post-media"].MaxBytes != 50*MB || b["chat-media"].MaxBytes != 25*MB || b["avatars"].MaxBytes != 3*MB {
		t.Errorf("bucket caps = %v %v %v", b["post-media"].MaxBytes, b["chat-media"].MaxBytes, b["avatars"].MaxBytes)
	}
	if len(Purposes()) != 8 {
		t.Errorf("purposes = %v", Purposes())
	}
}
