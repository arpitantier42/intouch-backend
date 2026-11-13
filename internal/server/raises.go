package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/intouch/api/internal/apierr"
	"github.com/intouch/api/internal/auth"
	"github.com/intouch/api/internal/payments"
	"github.com/jackc/pgx/v5"
)

// Message the client shows on Support while no gateway is configured.
const paymentsUnavailableMsg = "Contributions aren't available until payments are connected."

// registerRaises adds OP-071..074 (migration ..._raises_api.sql:
// create_raise, get_raise, close_raise, start_contribution).
func registerRaises(g Groups, d Deps) {
	h := &raiseHandlers{d: d}
	ready := (&meHandlers{d: d}).ready
	g.User.POST("/communities/:id/raises", ready, h.create)
	g.User.GET("/raises/:id", ready, h.get)
	g.User.POST("/raises/:id/close", ready, h.close)
	g.User.POST("/raises/:id/contributions", ready, h.contribute)
}

type raiseHandlers struct{ d Deps }

// raiseJSON runs sql as the caller and answers the raise with a `support`
// object: {enabled, mode gateway|unavailable|closed, provider, message}.
func (h *raiseHandlers) raiseJSON(c *gin.Context, status int, sql string, args ...any) {
	claims := auth.FromContext(c)
	var out []byte
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		return tx.QueryRow(c, sql, args...).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	var m map[string]any
	if json.Unmarshal(out, &m) != nil {
		writeErr(c, errors.New("malformed raise json"))
		return
	}
	m["support"] = h.support(m["is_open"] == true)
	c.JSON(status, m)
}

func (h *raiseHandlers) support(open bool) gin.H {
	switch {
	case !open:
		return gin.H{"enabled": false, "mode": "closed", "provider": nil, "message": "This raise is closed."}
	case h.d.Payments == nil:
		return gin.H{"enabled": false, "mode": "unavailable", "provider": nil, "message": paymentsUnavailableMsg}
	default:
		return gin.H{"enabled": true, "mode": "gateway", "provider": h.d.Payments.Provider(), "message": nil}
	}
}

type raiseBody struct {
	Title         *string         `json:"title"`
	Description   *string         `json:"description"`
	GoalMinor     json.RawMessage `json:"goal_minor"`
	OpenUntil     *string         `json:"open_until"`
	Visibility    *string         `json:"visibility"`
	InviteEnabled *bool           `json:"invite_enabled"`
	ReceiverName  *string         `json:"receiver_name"`
	PayoutMode    *string         `json:"payout_mode"`
	UPIID         *string         `json:"upi_id"`
	AccountNumber *string         `json:"account_number"`
	IFSC          *string         `json:"ifsc"`
	QRURL         *string         `json:"qr_url"`
}

var (
	upiRe     = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.]{2,}$`)
	accountRe = regexp.MustCompile(`^[0-9]{6,18}$`)
	ifscRe    = regexp.MustCompile(`^[A-Z0-9]{11}$`)
)

var raiseFieldOrder = []string{"title", "goal_minor", "open_until", "visibility", "receiver_name",
	"payout_mode", "upi_id", "account_number", "ifsc", "qr_url", "description"}

// jsonInt reads an optional JSON integer. ok=false for non-integers.
func jsonInt(raw json.RawMessage) (v *int64, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var n json.Number
	if dec.Decode(&n) != nil {
		return nil, false
	}
	i, err := n.Int64()
	if err != nil {
		return nil, false
	}
	return &i, true
}

// POST /communities/{id}/raises — 201 Raise (+ post_id, progress, support);
// 400 error.fields; 403 not joined.
func (h *raiseHandlers) create(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusForbidden, "Members only")
		return
	}
	var b raiseBody
	raw, _ := readBody(c)
	if raw == nil || json.Unmarshal(raw, &b) != nil {
		abortFields(c, map[string]string{"title": msgBadJSON}, "title")
		return
	}
	f := map[string]string{}
	if str(b.Title) == "" {
		f["title"] = "Add a title"
	} else if len([]rune(str(b.Title))) > 120 {
		f["title"] = "Title can be at most 120 characters"
	}
	if len([]rune(str(b.Description))) > 4000 {
		f["description"] = "Description can be at most 4000 characters"
	}
	goal, ok := jsonInt(b.GoalMinor)
	if !ok {
		f["goal_minor"] = "Enter the goal as a whole amount in paise"
	} else if goal != nil && *goal < 0 {
		f["goal_minor"] = "Goal can't be negative"
	}
	var until *time.Time
	if s := str(b.OpenUntil); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		switch {
		case err != nil:
			f["open_until"] = "Enter a valid date"
		case !t.After(time.Now()):
			f["open_until"] = "Open until must be in the future"
		default:
			until = &t
		}
	}
	vis := strings.ToLower(str(b.Visibility))
	if vis == "" {
		vis = "public"
	}
	if vis != "public" && vis != "private" {
		f["visibility"] = "Visibility must be public or private"
	}
	if str(b.ReceiverName) == "" {
		f["receiver_name"] = "Add who the money goes to"
	} else if len([]rune(str(b.ReceiverName))) > 120 {
		f["receiver_name"] = "Receiver name can be at most 120 characters"
	}
	mode := strings.ToLower(str(b.PayoutMode))
	upi, acct, ifsc := str(b.UPIID), str(b.AccountNumber), strings.ToUpper(str(b.IFSC))
	switch mode {
	case "upi":
		if upi == "" {
			f["upi_id"] = "Add a UPI ID"
		} else if !upiRe.MatchString(upi) || len(upi) > 100 {
			f["upi_id"] = "Enter a valid UPI ID, like name@bank"
		}
		acct, ifsc = "", ""
	case "bank":
		if acct == "" {
			f["account_number"] = "Add the account number"
		} else if !accountRe.MatchString(acct) {
			f["account_number"] = "Account number must be 6–18 digits"
		}
		if ifsc == "" {
			f["ifsc"] = "Add the IFSC code"
		} else if len(ifsc) != 11 {
			f["ifsc"] = "IFSC must be 11 characters"
		} else if !ifscRe.MatchString(ifsc) {
			f["ifsc"] = "IFSC can only contain letters and digits"
		}
		upi = ""
	default:
		f["payout_mode"] = "Choose UPI or Bank"
	}
	if q := str(b.QRURL); q != "" && !isHTTPURL(q) {
		f["qr_url"] = "QR image must be an http(s) link"
	}
	if len(f) > 0 {
		abortFields(c, f, raiseFieldOrder...)
		return
	}
	invite := b.InviteEnabled != nil && *b.InviteEnabled
	h.raiseJSON(c, http.StatusCreated,
		`select public.create_raise($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)::text`,
		c.Param("id"), str(b.Title), nilIfEmpty(str(b.Description)), goal, until, vis, invite,
		str(b.ReceiverName), mode, nilIfEmpty(upi), nilIfEmpty(acct), nilIfEmpty(ifsc), nilIfEmpty(str(b.QRURL)))
}

// GET /raises/{id} — 200 Raise via raises_masked (payout fields null unless
// organiser or owner/admin); 404 not visible.
func (h *raiseHandlers) get(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, "Raise not found")
		return
	}
	h.raiseJSON(c, http.StatusOK, `select public.get_raise($1)::text`, c.Param("id"))
}

// POST /raises/{id}/close — 200 Raise; 403 not the organiser; 404.
func (h *raiseHandlers) close(c *gin.Context) {
	if !uuidRe.MatchString(c.Param("id")) {
		apierr.Abort(c, http.StatusNotFound, "Raise not found")
		return
	}
	h.raiseJSON(c, http.StatusOK, `select public.close_raise($1)::text`, c.Param("id"))
}

// POST /raises/{id}/contributions {amount_minor} — checks the raise (404,
// 409 closed/past open_until, 400 amount), then 503 without a gateway, else
// creates the gateway order and stores a 'created' contribution keyed by the
// order id. 201 = Contribution row + `checkout` {provider, key_id, order_id,
// amount_minor, currency}.
func (h *raiseHandlers) contribute(c *gin.Context) {
	id := c.Param("id")
	if !uuidRe.MatchString(id) {
		apierr.Abort(c, http.StatusNotFound, "Raise not found")
		return
	}
	var b struct {
		AmountMinor json.RawMessage `json:"amount_minor"`
	}
	raw, _ := readBody(c)
	if raw != nil && json.Unmarshal(raw, &b) != nil {
		abortFields(c, map[string]string{"amount_minor": msgBadJSON}, "amount_minor")
		return
	}
	amount, ok := jsonInt(b.AmountMinor)
	if !ok || amount == nil || *amount < 100 {
		abortFields(c, map[string]string{"amount_minor": "Enter an amount of at least ₹1"}, "amount_minor")
		return
	}
	claims := auth.FromContext(c)
	var donor string
	err := h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		var x []byte
		return tx.QueryRow(c, `select public.start_contribution($1, $2, null)::text`, id, *amount).Scan(&x)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	if h.d.Payments == nil {
		apierr.Abort(c, http.StatusServiceUnavailable, paymentsUnavailableMsg)
		return
	}
	donor = claims.Subject
	order, err := h.d.Payments.CreateOrder(c, payments.Order{
		AmountMinor: *amount, Currency: "INR", Receipt: "raise_" + strings.ReplaceAll(id, "-", "")[:24],
		Notes: map[string]string{"raise_id": id, "donor_id": donor},
	})
	if err != nil {
		log.Printf("create order for raise %s: %v", id, err)
		apierr.Abort(c, http.StatusBadGateway, "The payment gateway is unavailable. Try again.")
		return
	}
	var out []byte
	err = h.d.DB.AsUser(c, claims.Raw, func(tx pgx.Tx) error {
		return tx.QueryRow(c, `select public.start_contribution($1, $2, $3)::text`, id, *amount, order.ID).Scan(&out)
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	m["checkout"] = gin.H{"provider": h.d.Payments.Provider(), "key_id": order.KeyID, "order_id": order.ID,
		"amount_minor": *amount, "currency": "INR"}
	c.JSON(http.StatusCreated, m)
}
