// Package gifs searches the GIF provider (GIPHY) on behalf of the app so the
// provider key never leaves the server. Handlers depend on the Searcher
// interface so tests can substitute a fake.
package gifs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the GIPHY API root.
const DefaultBaseURL = "https://api.giphy.com"

// Rating is the content rating passed to the provider for every search.
const Rating = "pg"

// GIF is one search result, reduced to what the picker and a gif message need.
type GIF struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	URL        string `json:"url"`         // full-size animation; send as attachment_url
	PreviewURL string `json:"preview_url"` // small animation for the picker grid
	Width      int    `json:"width"`
	Height     int    `json:"height"`
}

// Page is one page of results. NextOffset is nil on the last page.
type Page struct {
	Results    []GIF
	NextOffset *int
}

// Searcher runs a GIF search.
type Searcher interface {
	Search(ctx context.Context, q string, limit, offset int) (*Page, error)
}

// Error is a non-2xx provider response.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string { return fmt.Sprintf("gifs: HTTP %d: %s", e.Status, e.Body) }

// Giphy calls the GIPHY search API.
type Giphy struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

// New returns the GIPHY client with a short timeout: a slow provider must
// not hold the picker (or the server) for long.
func New(apiKey string) *Giphy {
	return &Giphy{BaseURL: DefaultBaseURL, APIKey: apiKey, Client: &http.Client{Timeout: 5 * time.Second}}
}

// Search calls GET /v1/gifs/search.
func (g *Giphy) Search(ctx context.Context, q string, limit, offset int) (*Page, error) {
	v := url.Values{
		"api_key": {g.APIKey},
		"q":       {q},
		"limit":   {strconv.Itoa(limit)},
		"offset":  {strconv.Itoa(offset)},
		"rating":  {Rating},
		"lang":    {"en"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(g.BaseURL, "/")+"/v1/gifs/search?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// The URL carries the key; never surface it in logs.
		return nil, fmt.Errorf("gifs: request failed")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, &Error{Status: resp.StatusCode, Body: snippet}
	}
	var out giphyResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("gifs: unexpected response")
	}
	page := &Page{Results: make([]GIF, 0, len(out.Data))}
	for _, d := range out.Data {
		full := d.Images.Original
		if full.URL == "" {
			full = d.Images.FixedHeight
		}
		if full.URL == "" {
			continue
		}
		prev := d.Images.FixedWidthSmall
		if prev.URL == "" {
			prev = d.Images.FixedHeight
		}
		if prev.URL == "" {
			prev = full
		}
		page.Results = append(page.Results, GIF{
			ID: d.ID, Title: d.Title, URL: full.URL, PreviewURL: prev.URL,
			Width: atoi(full.Width), Height: atoi(full.Height),
		})
	}
	next := out.Pagination.Offset + out.Pagination.Count
	if out.Pagination.Count > 0 && next < out.Pagination.TotalCount {
		page.NextOffset = &next
	}
	return page, nil
}

type giphyImage struct {
	URL    string `json:"url"`
	Width  string `json:"width"`
	Height string `json:"height"`
}

type giphyResponse struct {
	Data []struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Images struct {
			Original        giphyImage `json:"original"`
			FixedHeight     giphyImage `json:"fixed_height"`
			FixedWidthSmall giphyImage `json:"fixed_width_small"`
		} `json:"images"`
	} `json:"data"`
	Pagination struct {
		TotalCount int `json:"total_count"`
		Count      int `json:"count"`
		Offset     int `json:"offset"`
	} `json:"pagination"`
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
