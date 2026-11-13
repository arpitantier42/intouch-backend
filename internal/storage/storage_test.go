package storage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateSignedUploadURL(t *testing.T) {
	var gotPath, gotAuth, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotKey = r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("apikey")
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		w.Write([]byte(`{"url":"/object/upload/sign/avatars/u1/f.jpg?token=tok123"}`))
	}))
	defer srv.Close()

	s := New(srv.URL+"/", "svc")
	up, err := s.CreateSignedUploadURL(context.Background(), "avatars", "u1/f.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/storage/v1/object/upload/sign/avatars/u1/f.jpg" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer svc" || gotKey != "svc" {
		t.Errorf("auth headers = %q %q", gotAuth, gotKey)
	}
	if up.URL != srv.URL+"/storage/v1/object/upload/sign/avatars/u1/f.jpg?token=tok123" || up.Token != "tok123" {
		t.Errorf("upload = %+v", up)
	}
}

func TestCreateSignedUploadURLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"Bucket not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "svc").CreateSignedUploadURL(context.Background(), "nope", "a/b")
	var se *Error
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Fatalf("err = %v", err)
	}
}
