package server

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/gifs"
)

const (
	gifDefaultLimit = 24
	gifMaxLimit     = 50
	gifMaxQuery     = 50
	gifMaxOffset    = 4999 // GIPHY rejects larger offsets
	gifUnavailable  = "GIF search is unavailable right now. You can still send text."
)

// registerGifs adds OP-058. No DB: the handler only proxies the provider.
func registerGifs(g Groups, d Deps) {
	h := &gifHandlers{d: d}
	g.User.GET("/gifs", h.search)
}

type gifHandlers struct{ d Deps }

// GET /gifs?q=&limit=&offset= — 200 {query, results[], next_offset,
// attribution}. 400 blank/too long q or bad paging; 503 when the provider
// is not configured, fails or times out (the chat send path never calls it).
func (h *gifHandlers) search(c *gin.Context) {
	q := strings.Join(strings.Fields(c.Query("q")), " ")
	if q == "" {
		abortFields(c, map[string]string{"q": "Enter something to search for."}, "q")
		return
	}
	if utf8.RuneCountInString(q) > gifMaxQuery {
		abortFields(c, map[string]string{"q": "Search must be 50 characters or fewer."}, "q")
		return
	}
	limit, ok := intParam(c, "limit", gifDefaultLimit, 1, gifMaxLimit)
	if !ok {
		abortFields(c, map[string]string{"limit": "limit must be between 1 and 50."}, "limit")
		return
	}
	offset, ok := intParam(c, "offset", 0, 0, gifMaxOffset)
	if !ok {
		abortFields(c, map[string]string{"offset": "offset must be between 0 and 4999."}, "offset")
		return
	}
	if h.d.Gifs == nil {
		apierr.Abort(c, http.StatusServiceUnavailable, gifUnavailable)
		return
	}
	page, err := h.d.Gifs.Search(c.Request.Context(), q, limit, offset)
	if err != nil {
		var pe *gifs.Error
		if errors.As(err, &pe) {
			log.Printf("gifs: provider HTTP %d", pe.Status)
		} else {
			log.Printf("gifs: %v", err)
		}
		apierr.Abort(c, http.StatusServiceUnavailable, gifUnavailable)
		return
	}
	results := page.Results
	if results == nil {
		results = []gifs.GIF{}
	}
	c.JSON(http.StatusOK, gin.H{
		"query":       q,
		"results":     results,
		"next_offset": page.NextOffset,
		"attribution": "Powered by GIPHY",
	})
}

// intParam parses an optional integer query parameter within [min, max].
func intParam(c *gin.Context, name string, def, min, max int) (int, bool) {
	s := strings.TrimSpace(c.Query(name))
	if s == "" {
		return def, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < min || n > max {
		return 0, false
	}
	return n, true
}
