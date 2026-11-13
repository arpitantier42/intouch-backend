package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/push"
	"github.com/intouch/api/internal/uploads"
	"github.com/jackc/pgx/v5"
)

// registerJobs adds OP-075..080. Reads go through the jobs_feed view (jobs ∪
// job-tagged Home posts, filtered by membership and blocks inside the view);
// create/update are plain inserts/updates under RLS; delete and interests
// call the definer RPCs of migration ..._jobs.sql.
func registerJobs(g Groups, d Deps) {
	h := &jobHandlers{d: d}
	ready := (&meHandlers{d: d}).ready
	g.User.GET("/communities/:id/jobs", ready, h.list)
	g.User.POST("/communities/:id/jobs", ready, h.create)
	g.User.GET("/jobs/:id", ready, h.get)
	g.User.PATCH("/jobs/:id", ready, h.update)
	g.User.DELETE("/jobs/:id", ready, h.remove)
	g.User.POST("/jobs/:id/interests", ready, h.interest)
}

type jobHandlers struct{ d Deps }

const (
	jobTitleMsg    = "Add a role or title to post"
	jobApplyURLMsg = "Enter an http(s) apply link"
	jobNoteMax     = 280
	jobNotFound    = "Job not found"
	jobListDefault = 100
	jobListMax     = 200
)

// jobItemSQL decorates a jobs_feed row: mine, and can_message (Message
// candidate / Posted by → Message; never for one's own item).
const jobItemSQL = `to_jsonb(f) || jsonb_build_object(
  'mine', f.poster_id = auth.uid(),
  'can_message', f.poster_id <> auth.uid())`

// jobDetailSQL returns one full job (not a tagged post) as the caller sees
// it, plus the kinds of interest the caller already sent. No row = 404.
const jobDetailSQL = `select (` + jobItemSQL + ` || jsonb_build_object(
  'my_interests', coalesce((select jsonb_agg(i.kind order by i.kind) from public.job_interests i
                            where i.job_id = f.id and i.user_id = auth.uid()), '[]'::jsonb)))::text
from public.jobs_feed f where f.id = $1 and f.item_kind = 'job'`

// jobFieldMax mirrors the varchar limits of public.jobs.
var jobFieldMax = map[string]int{
	"title": 120, "company": 100, "location": 100, "experience": 60, "salary": 60, "details": 4000,
}

// jobColumns are the client-writable columns, in error display order.
var jobColumns = []string{"intent", "title", "company", "location", "job_type", "work_mode",
	"experience", "salary", "apply_url", "details"}

var jobEnums = map[string]map[string]bool{
	"intent":    {"opportunity": true, "looking": true},
	"job_type":  {"full_time": true, "part_time": true, "internship": true, "contract": true, "freelance": true},
	"work_mode": {"on_site": true, "remote": true, "hybrid": true},
}

var jobEnumMsg = map[string]string{
	"intent":    "Choose Opportunity or Looking.",
	"job_type":  "Choose Full-time, Part-time, Internship, Contract or Freelance.",
	"work_mode": "Choose On-site, Remote or Hybrid.",
}

// enumKey normalises "Full-time" / "On site" / "ON_SITE" to full_time / on_site.
func enumKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.NewReplacer("-", "_", " ", "_").Replace(s)
}

// parseJobFields validates the Job fields present in body. create=true
// requires a title and defaults intent to opportunity. Returns the column →
// value map (nil = SQL null) and field errors.
func parseJobFields(body map[string]json.RawMessage, create bool) (map[string]any, map[string]string) {
	vals := map[string]any{}
	fields := map[string]string{}
	for _, col := range jobColumns {
		raw, present := body[col]
		if !present {
			continue
		}
		if string(raw) == "null" {
			vals[col] = nil
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			fields[col] = col + " must be text."
			continue
		}
		s = strings.TrimSpace(s)
		if col == "details" {
			s = strings.TrimRight(s, " \t\r\n")
		}
		if s == "" {
			vals[col] = nil
			continue
		}
		if allowed, ok := jobEnums[col]; ok {
			s = enumKey(s)
			if !allowed[s] {
				fields[col] = jobEnumMsg[col]
				continue
			}
		}
		if max, ok := jobFieldMax[col]; ok && utf8.RuneCountInString(s) > max {
			fields[col] = fmt.Sprintf("Keep %s under %d characters.", strings.ReplaceAll(col, "_", " "), max)
			continue
		}
		if col == "apply_url" && !isHTTPURL(s) {
			fields[col] = jobApplyURLMsg
			continue
		}
		vals[col] = s
	}
	if v, present := vals["title"]; (create && !present) || (present && v == nil) {
		if _, bad := fields["title"]; !bad {
			fields["title"] = jobTitleMsg
		}
	}
	if v, present := vals["intent"]; present && v == nil {
		if create {
			delete(vals, "intent")
		} else {
			fields["intent"] = jobEnumMsg["intent"]
		}
	}
	if create {
		if _, ok := vals["intent"]; !ok {
			vals["intent"] = "opportunity"
		}
	}
	return vals, fields
}

func (h *jobHandlers) userTx(c *gin.Context, fn func(tx pgx.Tx) error) error {
	return h.d.DB.AsUser(c, auth.FromContext(c).Raw, fn)
}

// GET /communities/{id}/jobs?intent&q&since&sort[&limit] — 200 array of
// jobs_feed items (item_kind job|post, has_details false for tagged posts),
// 403 'Members only' unless joined. Main and community routes call this.
func (h *jobHandlers) list(c *gin.Context) {
	fields := map[string]string{}
	intent := enumKey(c.Query("intent"))
	if intent == "" {
		intent = "opportunity"
	} else if !jobEnums["intent"][intent] {
		fields["intent"] = jobEnumMsg["intent"]
	}
	var since *time.Time
	switch strings.ToLower(strings.TrimSpace(c.Query("since"))) {
	case "", "any":
	case "24h":
		t := time.Now().Add(-24 * time.Hour)
		since = &t
	case "week":
		t := time.Now().AddDate(0, 0, -7)
		since = &t
	case "month":
		t := time.Now().AddDate(0, -1, 0)
		since = &t
	default:
		fields["since"] = "Choose 24h, week or month."
	}
	dir := "desc"
	switch strings.ToLower(strings.TrimSpace(c.Query("sort"))) {
	case "", "newest":
	case "oldest":
		dir = "asc"
	default:
		fields["sort"] = "Choose newest or oldest."
	}
	limit := jobListDefault
	if s := c.Query("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > jobListMax {
			fields["limit"] = fmt.Sprintf("limit must be 1..%d.", jobListMax)
		} else {
			limit = n
		}
	}
	if len(fields) > 0 {
		abortFields(c, fields, "intent", "since", "sort", "limit")
		return
	}
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusForbidden, "Members only")
		return
	}
	q := strings.TrimSpace(c.Query("q"))
	// Opportunity: role, company, location. Looking: the person, the desired
	// role and location. strpos keeps user text out of LIKE patterns.
	search := `(strpos(lower(f.title), lower($3)) > 0 or strpos(lower(coalesce(f.company, '')), lower($3)) > 0
	            or strpos(lower(coalesce(f.location, '')), lower($3)) > 0)`
	if intent == "looking" {
		search = `(strpos(lower(f.poster_name), lower($3)) > 0 or strpos(lower(f.title), lower($3)) > 0
		           or strpos(lower(coalesce(f.location, '')), lower($3)) > 0)`
	}
	sql := `select coalesce(jsonb_agg(` + jobItemSQL + ` order by f.created_at ` + dir + `, f.id ` + dir + `), '[]'::jsonb)::text
	        from (select * from public.jobs_feed f
	              where f.community_id = $1 and f.intent = $2
	                and ($3 = '' or ` + search + `)
	                and ($4::timestamptz is null or f.created_at >= $4)
	              order by f.created_at ` + dir + `, f.id ` + dir + ` limit $5) f`
	var out []byte
	err := h.userTx(c, func(tx pgx.Tx) error {
		var joined bool
		if err := tx.QueryRow(c, `select public.is_joined($1)`, c.Param("id")).Scan(&joined); err != nil {
			return err
		}
		if !joined {
			return apierr.New(http.StatusForbidden, "Members only")
		}
		return tx.QueryRow(c, sql, c.Param("id"), intent, q, since, limit).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}

// POST /communities/{id}/jobs — 201 Job; 400 'Add a role or title to post',
// non-http(s) apply link or bad enums (error.fields); 403 not joined.
func (h *jobHandlers) create(c *gin.Context) {
	var body map[string]json.RawMessage
	if !bodyObject(c, &body) {
		return
	}
	vals, fields := parseJobFields(body, true)
	if len(fields) > 0 {
		abortFields(c, fields, jobColumns...)
		return
	}
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusForbidden, "Members only")
		return
	}
	cols := []string{"community_id"}
	args := []any{c.Param("id")}
	ph := []string{"$1"}
	for _, col := range jobColumns {
		if v, ok := vals[col]; ok {
			cols = append(cols, col)
			args = append(args, v)
			ph = append(ph, fmt.Sprintf("$%d", len(args)))
		}
	}
	claims := auth.FromContext(c)
	name, avatar := oauthMetadata(claims)
	var out []byte
	err := h.userTx(c, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, ensureProfileSQL, name, avatar); err != nil {
			return err
		}
		var id string
		if err := tx.QueryRow(c, `insert into public.jobs (`+strings.Join(cols, ", ")+`) values (`+
			strings.Join(ph, ", ")+`) returning id`, args...).Scan(&id); err != nil {
			return err
		}
		return tx.QueryRow(c, jobDetailSQL, id).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Data(http.StatusCreated, "application/json; charset=utf-8", out)
}

// GET /jobs/{id} — 200 Job detail (+ poster_name/avatar, mine, can_message,
// my_interests); 404 unknown, deleted, a post id, not joined, or blocked.
func (h *jobHandlers) get(c *gin.Context) {
	id, ok := pathID(c, jobNotFound)
	if !ok {
		return
	}
	var out []byte
	err := h.userTx(c, func(tx pgx.Tx) error { return tx.QueryRow(c, jobDetailSQL, id).Scan(&out) })
	if errors.Is(err, pgx.ErrNoRows) {
		apierr.Abort(c, http.StatusNotFound, jobNotFound)
		return
	}
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}

// PATCH /jobs/{id} — poster only. Client columns only (community, poster
// and timestamps are ignored). 200 Job, 400 validation, 403 not the poster,
// 404 not visible.
func (h *jobHandlers) update(c *gin.Context) {
	var body map[string]json.RawMessage
	if !bodyObject(c, &body) {
		return
	}
	id, ok := pathID(c, jobNotFound)
	if !ok {
		return
	}
	vals, fields := parseJobFields(body, false)
	if len(fields) > 0 {
		abortFields(c, fields, jobColumns...)
		return
	}
	uid := auth.FromContext(c).Subject
	var out []byte
	err := h.userTx(c, func(tx pgx.Tx) error {
		var poster string
		if err := tx.QueryRow(c, `select poster_id::text from public.jobs where id = $1`, id).Scan(&poster); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apierr.New(http.StatusNotFound, jobNotFound)
			}
			return err
		}
		if poster != uid {
			return apierr.New(http.StatusForbidden, "Only the poster can edit this job.")
		}
		var sets []string
		args := []any{id}
		for _, col := range jobColumns {
			if v, ok := vals[col]; ok {
				args = append(args, v)
				sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
			}
		}
		if len(sets) > 0 {
			tag, err := tx.Exec(c, `update public.jobs set `+strings.Join(sets, ", ")+` where id = $1`, args...)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return apierr.New(http.StatusForbidden, "Only the poster can edit this job.")
			}
		}
		return tx.QueryRow(c, jobDetailSQL, id).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}

// DELETE /jobs/{id} — 204 soft delete (leaves lists, Saved, detail);
// 403 not the poster, 404 not visible.
func (h *jobHandlers) remove(c *gin.Context) {
	id, ok := pathID(c, jobNotFound)
	if !ok {
		return
	}
	err := h.userTx(c, func(tx pgx.Tx) error {
		_, err := tx.Exec(c, `select public.delete_job($1)`, id)
		return err
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// POST /jobs/{id}/interests {kind interest|referral (default interest),
// note ≤280, resume_url .pdf|.docx} — 201 JobInterest; the poster gets a
// stored notification (in SQL) and a push (after commit). 400 note/kind/own
// job, 415 resume type, 404 not visible, 409 same kind already sent.
func (h *jobHandlers) interest(c *gin.Context) {
	var in struct {
		Kind      *string `json:"kind"`
		Note      *string `json:"note"`
		ResumeURL *string `json:"resume_url"`
	}
	if raw, _ := readBody(c); raw != nil && len(strings.TrimSpace(string(raw))) > 0 {
		if json.Unmarshal(raw, &in) != nil {
			apierr.Abort(c, http.StatusBadRequest, "Send a JSON object.")
			return
		}
	}
	kind := strings.ToLower(str(in.Kind))
	if kind == "" {
		kind = "interest"
	}
	fields := map[string]string{}
	if kind != "interest" && kind != "referral" {
		fields["kind"] = "Choose interest or referral."
	}
	note := str(in.Note)
	if utf8.RuneCountInString(note) > jobNoteMax {
		fields["note"] = "Keep your note under 280 characters."
	}
	if len(fields) > 0 {
		abortFields(c, fields, "kind", "note")
		return
	}
	resume := str(in.ResumeURL)
	if resume != "" && !isImageURL("resume", resume) { // extension check against the resume policy
		p, _ := uploads.Lookup("resume")
		apierr.Abort(c, http.StatusUnsupportedMediaType, p.BadType)
		return
	}
	id, ok := pathID(c, jobNotFound)
	if !ok {
		return
	}
	claims := auth.FromContext(c)
	name, avatar := oauthMetadata(claims)
	var out []byte
	var poster, title string
	err := h.userTx(c, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, ensureProfileSQL, name, avatar); err != nil {
			return err
		}
		if err := tx.QueryRow(c, `select public.submit_job_interest($1, $2, $3, $4)::text`,
			id, kind, nilIfEmpty(note), nilIfEmpty(resume)).Scan(&out); err != nil {
			return err
		}
		return tx.QueryRow(c, `select poster_id::text, title from public.jobs where id = $1`, id).Scan(&poster, &title)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	var row struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(out, &row)
	if h.d.Push != nil {
		n := push.Notification{
			UserIDs: []string{poster},
			Title:   "New interest in your job",
			Body:    "Someone is interested in " + title + ".",
			Data:    map[string]string{"kind": "job_interest", "job_id": id, "job_interest_id": row.ID, "interest_kind": kind},
		}
		if kind == "referral" {
			n.Title = "New referral request"
			n.Body = "Someone asked for a referral for " + title + "."
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = h.d.Push.Send(ctx, n)
		}()
	}
	c.Data(http.StatusCreated, "application/json; charset=utf-8", out)
}
