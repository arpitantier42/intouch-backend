package server

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// registerReports adds OP-044 (POST /reports) and OP-045 (GET /admin/reports).
// Rules live in `create_report` / `list_reports` (migration
// ..._feed_engagement.sql); neither ever returns reporter_id.
func registerReports(g Groups, d Deps) {
	h := &reportHandlers{socialHandlers{admissionHandlers{d: d}}}
	ready := (&meHandlers{d: d}).ready
	g.User.POST("/reports", ready, h.create)
	g.User.GET("/admin/reports", ready, h.list)
}

// reportHandlers reuses ensureThen: the reporter row needs a profile (FK),
// and a first request after Google sign-in may come before GET /me.
type reportHandlers struct{ socialHandlers }

const reportDetailsMax = 1000

var (
	reportKinds    = map[string]bool{"post": true, "comment": true, "thread": true, "job": true, "user": true}
	reportReasons  = map[string]bool{"spam": true, "inappropriate": true, "fake_job": true, "harassment": true, "other": true}
	reportStatuses = map[string]bool{"open": true, "actioned": true, "dismissed": true}
)

// POST /reports {target_kind, target_id, reason, details?} — 201 Report with
// status open and no reporter_id (the reporter is always the caller; a
// client-sent reporter_id is ignored); 400 no/unknown reason, bad target or
// details over 1000 characters.
func (h *reportHandlers) create(c *gin.Context) {
	var in struct {
		TargetKind string  `json:"target_kind"`
		TargetID   string  `json:"target_id"`
		Reason     string  `json:"reason"`
		Details    *string `json:"details"`
	}
	if !bodyObject(c, &in) {
		return
	}
	fields := map[string]string{}
	if !reportReasons[in.Reason] {
		fields["reason"] = "Choose a reason."
	}
	if !reportKinds[in.TargetKind] {
		fields["target_kind"] = "target_kind must be post, comment, thread, job or user."
	}
	if !uuidRe.MatchString(in.TargetID) {
		fields["target_id"] = "target_id must be a UUID."
	}
	var details any
	if in.Details != nil {
		d := strings.TrimSpace(*in.Details)
		if utf8.RuneCountInString(d) > reportDetailsMax {
			fields["details"] = "Details can be up to 1000 characters."
		} else if d != "" {
			details = d
		}
	}
	if len(fields) > 0 {
		abortFields(c, fields, "reason", "target_kind", "target_id", "details")
		return
	}
	h.ensureThen(c, http.StatusCreated, `select public.create_report($1, $2, $3, $4)`,
		in.TargetKind, in.TargetID, in.Reason, details)
}

// GET /admin/reports?status=open|actioned|dismissed — 200 array (newest
// first) for operators (profiles.is_operator); 403 'Operators only' for
// everyone else; 400 unknown status.
func (h *reportHandlers) list(c *gin.Context) {
	var status any
	if s := c.Query("status"); s != "" {
		if !reportStatuses[s] {
			abortFields(c, map[string]string{"status": "status must be open, actioned or dismissed."}, "status")
			return
		}
		status = s
	}
	h.rpcJSON(c, http.StatusOK, `select public.list_reports($1)`, status)
}
