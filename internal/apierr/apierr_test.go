package apierr

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestStatus(t *testing.T) {
	pg := func(code, msg string) error {
		return fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code, Message: msg})
	}
	cases := []struct {
		err    error
		status int
		msg    string
	}{
		{pg("PT403", "Admins only"), 403, "Admins only"},
		{pg("PT404", "No pending request"), 404, "No pending request"},
		{pg("PT409", "Event full"), 409, "Event full"},
		{pg("PT410", "Expired"), 410, "Expired"},
		{pg("PT423", "Locked"), 423, "Locked"},
		{pg("PT429", "Too soon"), 429, "Too soon"},
		{pg("PTxyz", "bad"), 500, "Internal server error"},
		{pg("23514", "check"), 400, "Invalid input"},
		{pg("22001", "too long"), 400, "Invalid input"},
		{pg("23505", "dup"), 409, "Already exists"},
		{pg("42501", "rls"), 403, "Forbidden"},
		{pg("23503", "fk"), 404, "Not found"},
		{pgx.ErrNoRows, 404, "Not found"},
		{New(415, "Logo must be an image"), 415, "Logo must be an image"},
		{errors.New("boom: secret detail"), 500, "Internal server error"},
	}
	for _, tc := range cases {
		s, m := Status(tc.err)
		if s != tc.status || m != tc.msg {
			t.Errorf("%v: got %d %q, want %d %q", tc.err, s, m, tc.status, tc.msg)
		}
	}
}
