package gifs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const giphyBody = `{"data":[
 {"id":"a1","title":"Party Parrot","images":{
   "original":{"url":"https://media.giphy.com/a1.gif","width":"480","height":"270"},
   "fixed_width_small":{"url":"https://media.giphy.com/a1_s.gif","width":"100","height":"56"}}},
 {"id":"a2","title":"Dance party","images":{
   "fixed_height":{"url":"https://media.giphy.com/a2_h.gif","width":"356","height":"200"}}},
 {"id":"a3","title":"broken","images":{}}],
 "pagination":{"total_count":120,"count":3,"offset":0}}`

func TestGiphySearch(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte(giphyBody))
	}))
	defer srv.Close()
	g := &Giphy{BaseURL: srv.URL, APIKey: "secret-key", Client: srv.Client()}
	page, err := g.Search(context.Background(), "party", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	q := got.URL.Query()
	if got.URL.Path != "/v1/gifs/search" || q.Get("q") != "party" || q.Get("api_key") != "secret-key" ||
		q.Get("limit") != "3" || q.Get("offset") != "0" || q.Get("rating") != Rating {
		t.Fatalf("request: %s", got.URL)
	}
	if len(page.Results) != 2 {
		t.Fatalf("results: %+v", page.Results)
	}
	a := page.Results[0]
	if a.ID != "a1" || a.URL != "https://media.giphy.com/a1.gif" || a.PreviewURL != "https://media.giphy.com/a1_s.gif" || a.Width != 480 || a.Height != 270 {
		t.Fatalf("first: %+v", a)
	}
	b := page.Results[1]
	if b.URL != "https://media.giphy.com/a2_h.gif" || b.PreviewURL != b.URL {
		t.Fatalf("fallbacks: %+v", b)
	}
	if page.NextOffset == nil || *page.NextOffset != 3 {
		t.Fatalf("next: %v", page.NextOffset)
	}
}

func TestGiphyLastPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[],"pagination":{"total_count":0,"count":0,"offset":0}}`))
	}))
	defer srv.Close()
	page, err := (&Giphy{BaseURL: srv.URL, APIKey: "k"}).Search(context.Background(), "zzz", 10, 0)
	if err != nil || len(page.Results) != 0 || page.NextOffset != nil {
		t.Fatalf("%+v %v", page, err)
	}
}

func TestGiphyErrorsHideKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	g := &Giphy{BaseURL: srv.URL, APIKey: "secret-key", Client: srv.Client()}
	_, err := g.Search(context.Background(), "party", 5, 0)
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != http.StatusBadGateway {
		t.Fatalf("status error: %v", err)
	}
	srv.Close() // now unreachable: transport error
	_, err = g.Search(context.Background(), "party", 5, 0)
	if err == nil || strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("transport error leaks key or is nil: %v", err)
	}
}
