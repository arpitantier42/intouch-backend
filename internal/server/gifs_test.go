package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/config"
	"github.com/intouch/api/internal/db"
	"github.com/intouch/api/internal/gifs"
)

// fakeGifs is a gifs.Searcher; err simulates the provider being down.
type fakeGifs struct {
	queries []string
	err     error
}

func (f *fakeGifs) Search(_ context.Context, q string, limit, offset int) (*gifs.Page, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	all := []gifs.GIF{
		{ID: "p1", Title: "Party parrot", URL: "https://media.giphy.com/p1.gif", PreviewURL: "https://media.giphy.com/p1_s.gif", Width: 480, Height: 270},
		{ID: "p2", Title: "Office party", URL: "https://media.giphy.com/p2.gif", PreviewURL: "https://media.giphy.com/p2_s.gif", Width: 320, Height: 240},
		{ID: "c1", Title: "Cat", URL: "https://media.giphy.com/c1.gif"},
	}
	out := []gifs.GIF{}
	for _, g := range all {
		if strings.Contains(strings.ToLower(g.Title), strings.ToLower(q)) {
			out = append(out, g)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return &gifs.Page{Results: out}, nil
}

func gifEngine(s gifs.Searcher) *gin.Engine {
	gin.SetMode(gin.TestMode)
	return New(Deps{Config: config.Config{JWTSecret: secret}, Gifs: s})
}

func getGifs(t *testing.T, r http.Handler, query string, bearer bool) (int, map[string]any) {
	t.Helper()
	h := map[string]string{}
	if bearer {
		h["Authorization"] = "Bearer " + token(uploader, time.Now().Add(time.Hour))
	}
	w := do(t, r, http.MethodGet, "/gifs?"+query, h)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func TestGifSearchReturnsMatches(t *testing.T) {
	f := &fakeGifs{}
	r := gifEngine(f)
	code, body := getGifs(t, r, "q=party", true)
	if code != 200 {
		t.Fatalf("party: %d %v", code, body)
	}
	res, _ := body["results"].([]any)
	if len(res) != 2 {
		t.Fatalf("results: %v", body)
	}
	for _, x := range res {
		m := x.(map[string]any)
		if !strings.Contains(strings.ToLower(m["title"].(string)), "party") || m["url"] == "" || m["preview_url"] == "" {
			t.Fatalf("result: %v", m)
		}
	}
	if body["query"] != "party" || body["attribution"] != "Powered by GIPHY" {
		t.Fatalf("envelope: %v", body)
	}
	if _, ok := body["next_offset"]; !ok {
		t.Fatalf("next_offset missing: %v", body)
	}
	// The query is trimmed and whitespace-collapsed before it reaches the provider.
	if code, _ := getGifs(t, r, "q="+url.QueryEscape("  office   party "), true); code != 200 || f.queries[len(f.queries)-1] != "office party" {
		t.Fatalf("normalised q: %d %v", code, f.queries)
	}
	// No matches → empty array, not null.
	code, body = getGifs(t, r, "q=zebra", true)
	if res, ok := body["results"].([]any); code != 200 || !ok || len(res) != 0 {
		t.Fatalf("empty: %d %v", code, body)
	}
}

func TestGifSearchValidation(t *testing.T) {
	f := &fakeGifs{}
	r := gifEngine(f)
	if code, _ := getGifs(t, r, "q=party", false); code != 401 {
		t.Fatalf("no bearer: %d", code)
	}
	for _, q := range []string{"", "q=", "q=%20%20", "q=" + strings.Repeat("a", 51), "q=x&limit=0", "q=x&limit=51", "q=x&limit=ab", "q=x&offset=-1", "q=x&offset=5000"} {
		if code, body := getGifs(t, r, q, true); code != 400 {
			t.Errorf("%q → %d %v", q, code, body)
		}
	}
	if len(f.queries) != 0 {
		t.Fatalf("provider called on invalid input: %v", f.queries)
	}
}

func TestGifProviderDownIs503(t *testing.T) {
	for name, s := range map[string]gifs.Searcher{
		"not configured": nil,
		"http error":     &fakeGifs{err: &gifs.Error{Status: 500, Body: "down"}},
		"transport":      &fakeGifs{err: errors.New("dial tcp: timeout")},
	} {
		code, body := getGifs(t, gifEngine(s), "q=party", true)
		if code != 503 || errMsg(body) != gifUnavailable {
			t.Errorf("%s: %d %v", name, code, body)
		}
	}
}

// With the GIF provider down, GIF search fails with 503 but sending a text
// (and a GIF picked earlier) still works: the send path never calls it.
func TestTextSendingSurvivesGifOutage(t *testing.T) {
	e := newSessEnv(t)
	down := &fakeGifs{err: &gifs.Error{Status: 503, Body: "maintenance"}}
	e.r = New(Deps{Config: config.Config{JWTSecret: secret}, DB: &db.DB{Pool: e.td.Pool}, Auth: e.fake, Push: &fakePush{}, Gifs: down})
	alice, aTok := admUser(t, e, "alice@example.com")
	bob, _ := admUser(t, e, "bob@example.com")
	cid, _ := admCommunity(t, e, alice, "Alpha", "private")
	chatJoin(t, e, cid, bob)
	_, dm := e.raw(t, "POST", "/communities/"+cid+"/threads/direct?user_id="+bob, aTok, nil)
	tid := field(dm, "id").(string)

	if code, _ := e.raw(t, "GET", "/gifs?q=party", aTok, nil); code != 503 {
		t.Fatalf("gif search: %d", code)
	}
	if code, out := send(t, e, tid, cid, aTok, text("party tonight?")); code != 201 || field(out, "body") != "party tonight?" {
		t.Fatalf("text send during outage: %d %v", code, out)
	}
	if code, out := send(t, e, tid, cid, aTok, map[string]any{"kind": "gif", "attachment_url": "https://media.giphy.com/p1.gif"}); code != 201 {
		t.Fatalf("gif send during outage: %d %v", code, out)
	}
}
