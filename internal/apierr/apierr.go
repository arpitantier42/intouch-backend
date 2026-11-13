// Package apierr maps errors (Postgres SQLSTATEs from RPCs and constraints)
// to HTTP statuses and writes the shared JSON error body:
//
//	{"error": {"status": 403, "code": "forbidden", "message": "..."}}
package apierr

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Error is an error with an HTTP status and a client-safe message.
type Error struct {
	Status  int
	Message string
	Err     error // underlying cause, never sent to clients
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// New returns an Error with the given status and message.
func New(status int, message string) *Error { return &Error{Status: status, Message: message} }

// Status returns the HTTP status for err and the message safe to show.
//
//   - *Error: its own status and message.
//   - SQLSTATE PTnnn (raised by RPCs): status nnn, the RPC's message.
//   - 23514 check, 22001 too long, 22P02/22023/22007/22008 bad input: 400.
//   - 23505 unique: 409. 23503 foreign key: 404 (referenced row missing).
//   - 42501 privilege or RLS violation: 403.
//   - pgx.ErrNoRows: 404.
//   - anything else: 500 with a generic message.
func Status(err error) (int, string) {
	var e *Error
	if errors.As(err, &e) {
		return e.Status, e.Message
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return http.StatusNotFound, "Not found"
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		if strings.HasPrefix(pg.Code, "PT") {
			if n, convErr := strconv.Atoi(pg.Code[2:]); convErr == nil && n >= 400 && n < 600 {
				return n, pg.Message
			}
		}
		switch pg.Code {
		case "23514", "23502", "22001", "22P02", "22023", "22007", "22008", "22003":
			return http.StatusBadRequest, "Invalid input"
		case "23505":
			return http.StatusConflict, "Already exists"
		case "23503":
			return http.StatusNotFound, "Not found"
		case "42501":
			return http.StatusForbidden, "Forbidden"
		case "40001", "40P01":
			return http.StatusConflict, "Conflict, try again"
		}
	}
	return http.StatusInternalServerError, "Internal server error"
}

// Write aborts the request with the JSON error body for err.
func Write(c *gin.Context, err error) {
	status, msg := Status(err)
	if status >= 500 {
		_ = c.Error(err) // keep the cause for logging
	}
	c.AbortWithStatusJSON(status, Body(status, msg))
}

// Abort writes an error with an explicit status and message.
func Abort(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, Body(status, message))
}

// Body is the error response body.
func Body(status int, message string) gin.H {
	code := strings.ToLower(strings.ReplaceAll(http.StatusText(status), " ", "_"))
	return gin.H{"error": gin.H{"status": status, "code": code, "message": message}}
}
