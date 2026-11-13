package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/auth"
	"github.com/jackc/pgx/v5"
)

const (
	notificationsDefaultLimit = 50
	notificationsMaxLimit     = 100
)

// registerNotifications adds OP-090: the top-bar bell inbox.
func registerNotifications(g Groups, d Deps) {
	h := &meHandlers{d: d}
	g.User.GET("/me/notifications", h.ready, h.listNotifications)
}

// Rows are read under RLS (notifications_select_own), newest first.
// Optional filters: unread=true, before=<RFC3339 created_at of the last row
// seen> for paging, limit 1..100 (default 50).
const listNotificationsSQL = `
select coalesce(jsonb_agg(to_jsonb(n) order by n.created_at desc, n.id desc), '[]'::jsonb)::text
from (
	select id, user_id, kind, payload, read_at, created_at
	from public.notifications
	where user_id = auth.uid()
	  and ($1::boolean is not true or read_at is null)
	  and ($2::timestamptz is null or created_at < $2)
	order by created_at desc, id desc
	limit $3
) n`

const unreadNotificationsSQL = `
select count(*) from public.notifications where user_id = auth.uid() and read_at is null`

// GET /me/notifications — the caller's notifications as a JSON array.
// X-Unread-Count carries the unread total for the bell badge.
func (h *meHandlers) listNotifications(c *gin.Context) {
	errs := map[string]string{}
	unread := false
	if v := strings.TrimSpace(c.Query("unread")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs["unread"] = "unread must be true or false."
		}
		unread = b
	}
	var before *time.Time
	if v := strings.TrimSpace(c.Query("before")); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			errs["before"] = "before must be an RFC 3339 timestamp."
		} else {
			before = &t
		}
	}
	limit := notificationsDefaultLimit
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > notificationsMaxLimit {
			errs["limit"] = "limit must be between 1 and 100."
		} else {
			limit = n
		}
	}
	if len(errs) > 0 {
		abortFields(c, errs, "unread", "before", "limit")
		return
	}

	claims := auth.FromContext(c)
	var body string
	var unreadCount int64
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		if err := tx.QueryRow(c, listNotificationsSQL, unread, before, limit).Scan(&body); err != nil {
			return err
		}
		return tx.QueryRow(c, unreadNotificationsSQL).Scan(&unreadCount)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Header("X-Unread-Count", strconv.FormatInt(unreadCount, 10))
	c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(body))
}
