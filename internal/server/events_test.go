package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func eventBodyFor(start time.Time) map[string]any {
	return map[string]any{
		"title": "Founders meetup", "starts_at": start.Format(time.RFC3339),
		"format": "in_person", "venue": "Koramangala Social", "tags": []string{"startup", "networking"},
	}
}

func createEventAs(t *testing.T, e *rsvpEnv, tok, cid string, body map[string]any) map[string]any {
	t.Helper()
	code, out := e.raw(t, "POST", "/communities/"+cid+"/events", tok, body)
	if code != 201 {
		t.Fatalf("create event: %d %v", code, out)
	}
	return out.(map[string]any)
}

func listIDs(t *testing.T, e *rsvpEnv, tok, path string) []string {
	t.Helper()
	code, out := e.raw(t, "GET", path, tok, nil)
	if code != 200 {
		t.Fatalf("%s: %d %v", path, code, out)
	}
	var ids []string
	for _, x := range out.([]any) {
		ids = append(ids, field(x, "id").(string))
	}
	return ids
}

func TestCreateEventValidationAndFeedPost(t *testing.T) {
	e := newRsvpEnv(t)
	host, hTok := admUser(t, e.sessEnv, "host@example.com")
	cid, _ := admCommunity(t, e.sessEnv, host, "Alpha", "private")
	_, mTok := rsvpUser(t, e, cid, "asha@example.com")
	start := time.Now().Add(72 * time.Hour).Truncate(time.Hour).UTC()

	for name, tc := range map[string]struct {
		mut   func(b map[string]any)
		field string
		msg   string
	}{
		"no title":   {func(b map[string]any) { b["title"] = "  " }, "title", "Add an event title"},
		"no date":    {func(b map[string]any) { delete(b, "starts_at") }, "starts_at", "Fill date, time and location"},
		"no venue":   {func(b map[string]any) { delete(b, "venue") }, "venue", "Fill date, time and location"},
		"end before": {func(b map[string]any) { b["ends_at"] = start.Add(-time.Hour).Format(time.RFC3339) }, "ends_at", "End must be after start"},
		"seat cap 0": {func(b map[string]any) { b["seat_cap"] = 0 }, "seat_cap", numbersMsg},
		"price abc": {func(b map[string]any) {
			b["is_paid"] = true
			b["price_minor"] = "abc"
			b["payment_url"] = "https://pay.example.com"
		}, "price_minor", numbersMsg},
		"paid no link": {func(b map[string]any) { b["is_paid"] = true; b["price_minor"] = 50000 }, "payment_url", "Add an http(s) payment link"},
		"paid ftp link": {func(b map[string]any) {
			b["is_paid"] = true
			b["price_minor"] = 50000
			b["payment_url"] = "ftp://x.example.com"
		}, "payment_url", "Add an http(s) payment link"},
		"online no link": {func(b map[string]any) { b["format"] = "online"; delete(b, "venue") }, "join_url", "Fill date, time and location"},
	} {
		b := eventBodyFor(start)
		tc.mut(b)
		code, out := e.raw(t, "POST", "/communities/"+cid+"/events", hTok, b)
		errObj, _ := field(out, "error").(map[string]any)
		fields, _ := errObj["fields"].(map[string]any)
		if code != 400 || fields[tc.field] != tc.msg {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	// Nothing was created by the rejected publishes.
	var n int
	_ = e.td.Pool.QueryRow(context.Background(), `select count(*) from public.events`).Scan(&n)
	if n != 0 {
		t.Fatalf("rejected publishes created %d events", n)
	}

	// Not joined → 403.
	_, outsider := admUser(t, e.sessEnv, "out@example.com")
	if code, _ := e.raw(t, "POST", "/communities/"+cid+"/events", outsider, eventBodyFor(start)); code != 403 {
		t.Fatalf("outsider create: %d", code)
	}

	// Published: end auto-fills to +2h, badge, feed post visible to another member.
	ev := createEventAs(t, e, hTok, cid, eventBodyFor(start))
	ends, _ := time.Parse(time.RFC3339, ev["ends_at"].(string))
	if !ends.Equal(start.Add(2*time.Hour)) || ev["badge"] != "Created by you" || ev["duration_minutes"].(float64) != 120 {
		t.Fatalf("created: %v", ev)
	}
	postID, _ := ev["post_id"].(string)
	if postID == "" || !contains(feedIDs(t, e.sessEnv, mTok, "/communities/"+cid+"/posts"), postID) {
		t.Fatalf("event post not in member feed: %v", ev)
	}
}

func TestListEventsTabsSearchAndBadges(t *testing.T) {
	e := newRsvpEnv(t)
	host, hTok := admUser(t, e.sessEnv, "host@example.com")
	cid, _ := admCommunity(t, e.sessEnv, host, "Alpha", "private")
	other, _ := admCommunity(t, e.sessEnv, host, "Beta", "private")
	_, mTok := rsvpUser(t, e, cid, "asha@example.com")
	start := time.Now().Add(72 * time.Hour).Truncate(time.Hour).UTC()

	free := createEventAs(t, e, hTok, cid, eventBodyFor(start))["id"].(string)
	pb := eventBodyFor(start.Add(time.Hour))
	pb["title"], pb["venue"] = "Paid workshop", "Indiranagar Hall"
	pb["is_paid"], pb["price_minor"], pb["payment_url"] = true, 50000, "https://pay.example.com/w"
	paid := createEventAs(t, e, hTok, cid, pb)["id"].(string)
	ob := eventBodyFor(start)
	ob["venue"] = "Indiranagar Hall"
	elsewhere := createEventAs(t, e, hTok, other, ob)["id"].(string)

	base := "/communities/" + cid + "/events"
	if ids := listIDs(t, e, mTok, base); len(ids) != 2 {
		t.Fatalf("all: %v", ids)
	}
	if ids := listIDs(t, e, mTok, base+"?tab=paid"); len(ids) != 1 || ids[0] != paid {
		t.Fatalf("paid: %v", ids)
	}
	// Venue-only match, and the other community's matching event is excluded.
	if ids := listIDs(t, e, hTok, base+"?q=indiranagar"); len(ids) != 1 || ids[0] != paid || contains(ids, elsewhere) {
		t.Fatalf("venue search: %v", ids)
	}
	if ids := listIDs(t, e, mTok, base+"?q=NETWORK"); len(ids) != 2 {
		t.Fatalf("tag search: %v", ids)
	}
	if ids := listIDs(t, e, mTok, base+"?tab=my_rsvps"); len(ids) != 0 {
		t.Fatalf("my_rsvps before RSVP: %v", ids)
	}
	if code, _ := e.raw(t, "PUT", "/events/"+paid+"/rsvp", mTok, map[string]any{"status": "going", "payment_status": "pay_later"}); code != 200 {
		t.Fatalf("rsvp: %d", code)
	}
	code, out := e.raw(t, "GET", base+"?tab=my_rsvps", mTok, nil)
	items := out.([]any)
	if code != 200 || len(items) != 1 || field(items[0], "id") != paid || field(items[0], "badge") != "Payment pending" {
		t.Fatalf("my_rsvps: %d %v", code, out)
	}
	if ids := listIDs(t, e, hTok, base+"?tab=created"); len(ids) != 2 {
		t.Fatalf("created: %v", ids)
	}
	_, out = e.raw(t, "GET", base+"?tab=created", hTok, nil)
	if field(out.([]any)[0], "badge") != "Created by you" {
		t.Fatalf("badge: %v", out)
	}
	_ = free
	_, outsider := admUser(t, e.sessEnv, "out@example.com")
	if code, _ := e.raw(t, "GET", base, outsider, nil); code != 403 {
		t.Fatalf("outsider list: %d", code)
	}
	if code, _ := e.raw(t, "GET", base+"?tab=raise", mTok, nil); code != 200 {
		t.Fatalf("raise tab: %d", code)
	}
}

func TestGetEventJoinURLAndLanding(t *testing.T) {
	e := newRsvpEnv(t)
	host, hTok := admUser(t, e.sessEnv, "host@example.com")
	pub, _ := admCommunity(t, e.sessEnv, host, "Open Club", "public")
	priv, _ := admCommunity(t, e.sessEnv, host, "Closed Club", "private")
	_, mTok := rsvpUser(t, e, pub, "asha@example.com")
	start := time.Now().Add(72 * time.Hour).Truncate(time.Hour).UTC()

	b := eventBodyFor(start)
	delete(b, "venue")
	b["format"], b["join_url"] = "online", "https://meet.example.com/x"
	b["is_paid"], b["price_minor"], b["payment_url"] = true, 50000, "https://pay.example.com/w"
	ev := createEventAs(t, e, hTok, pub, b)["id"].(string)

	code, out := e.raw(t, "GET", "/events/"+ev, hTok, nil)
	if code != 200 || field(out, "join_url") != "https://meet.example.com/x" {
		t.Fatalf("creator detail: %d %v", code, out)
	}
	e.raw(t, "PUT", "/events/"+ev+"/rsvp", mTok, map[string]any{"status": "going", "payment_status": "pay_later"})
	code, out = e.raw(t, "GET", "/events/"+ev, mTok, nil)
	if code != 200 || field(out, "join_url") != nil || len(field(out, "attendees").([]any)) != 1 {
		t.Fatalf("unconfirmed paid detail: %d %v", code, out)
	}
	_, outsider := admUser(t, e.sessEnv, "out@example.com")
	if code, _ := e.raw(t, "GET", "/events/"+ev, outsider, nil); code != 403 {
		t.Fatalf("non-member detail: %d", code)
	}
	if code, _ := e.raw(t, "GET", "/events/00000000-0000-0000-0000-000000000000", mTok, nil); code != 404 {
		t.Fatalf("unknown detail: %d", code)
	}

	// Landing: signed out renders the preview without links and grants nothing.
	code, out = e.raw(t, "GET", "/events/"+ev+"/join", "", nil)
	if code != 200 || field(out, "where") != "Online" || field(out, "going_count").(float64) != 1 ||
		field(field(out, "host"), "id") != host || field(out, "join_url") != nil || field(out, "payment_url") != nil {
		t.Fatalf("landing: %d %v", code, out)
	}
	code, out = e.raw(t, "GET", "/events/"+ev+"/join", outsider, nil)
	if code != 200 || field(out, "joined") != false {
		t.Fatalf("landing outsider: %d %v", code, out)
	}
	if code, _ := e.raw(t, "GET", "/events/"+ev, outsider, nil); code != 403 {
		t.Fatalf("landing granted access: %d", code)
	}
	pe := createEventAs(t, e, hTok, priv, eventBodyFor(start))["id"].(string)
	if code, out := e.raw(t, "GET", "/events/"+pe+"/join", "", nil); code != 403 || strings.Contains(fmt.Sprint(out), "Founders") {
		t.Fatalf("private landing: %d %v", code, out)
	}
	for _, id := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		if code, _ := e.raw(t, "GET", "/events/"+id+"/join", "", nil); code != 404 {
			t.Fatalf("landing %s: %d", id, code)
		}
	}
	// Feed URLs still require sign-in.
	if code, _ := e.raw(t, "GET", "/communities/"+pub+"/posts", "", nil); code != 401 {
		t.Fatalf("feed signed out: %d", code)
	}
}

func TestUpdateDeleteEventAndReminder(t *testing.T) {
	e := newRsvpEnv(t)
	host, hTok := admUser(t, e.sessEnv, "host@example.com")
	cid, _ := admCommunity(t, e.sessEnv, host, "Alpha", "private")
	asha, mTok := rsvpUser(t, e, cid, "asha@example.com")
	ravi, rTok := rsvpUser(t, e, cid, "ravi@example.com")
	start := time.Now().Add(72 * time.Hour).Truncate(time.Hour).UTC()
	ev := createEventAs(t, e, hTok, cid, eventBodyFor(start))["id"].(string)

	if code, _ := e.raw(t, "PATCH", "/events/"+ev, mTok, map[string]any{"title": "Hijacked"}); code != 403 {
		t.Fatalf("non-creator edit: %d", code)
	}
	code, out := e.raw(t, "PATCH", "/events/"+ev, hTok, map[string]any{"title": "Founders night", "live_state": "live", "creator_id": asha})
	if code != 200 || field(out, "title") != "Founders night" || field(out, "live_state") != "live" || field(out, "creator_id") != host {
		t.Fatalf("creator edit: %d %v", code, out)
	}
	if code, _ := e.raw(t, "PATCH", "/events/"+ev, hTok, map[string]any{"ends_at": start.Add(-time.Hour).Format(time.RFC3339)}); code != 400 {
		t.Fatalf("bad edit: %d", code)
	}

	// Remind me.
	code, out = e.raw(t, "POST", "/events/"+ev+"/reminders", mTok, map[string]any{})
	if code != 201 || field(out, "channel") != "push" || field(out, "user_id") != asha {
		t.Fatalf("remind: %d %v", code, out)
	}
	if code, _ := e.raw(t, "POST", "/events/"+ev+"/reminders", mTok, nil); code != 201 {
		t.Fatalf("remind again: %d", code)
	}
	_, outsider := admUser(t, e.sessEnv, "out@example.com")
	if code, _ := e.raw(t, "POST", "/events/"+ev+"/reminders", outsider, nil); code != 404 {
		t.Fatalf("outsider remind: %d", code)
	}

	// Delete: only the creator; going attendees notified and pushed.
	e.raw(t, "PUT", "/events/"+ev+"/rsvp", mTok, map[string]any{"status": "going"})
	e.raw(t, "PUT", "/events/"+ev+"/rsvp", rTok, map[string]any{"status": "not_sure"})
	if code, _ := e.raw(t, "DELETE", "/events/"+ev, mTok, nil); code != 403 {
		t.Fatalf("non-creator delete: %d", code)
	}
	if code, _ := e.raw(t, "DELETE", "/events/"+ev, hTok, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	var n int
	_ = e.td.Pool.QueryRow(context.Background(),
		`select count(*) from public.notifications where kind = 'event_deleted' and payload->>'event_id' = $1 and user_id = $2`, ev, asha).Scan(&n)
	var nr int
	_ = e.td.Pool.QueryRow(context.Background(),
		`select count(*) from public.notifications where kind = 'event_deleted' and user_id = $1`, ravi).Scan(&nr)
	if n != 1 || nr != 0 {
		t.Fatalf("notifications: going=%d not_sure=%d", n, nr)
	}
	if p := e.push.last(t, 1); p.Data["kind"] != "event_deleted" || len(p.UserIDs) != 1 || p.UserIDs[0] != asha {
		t.Fatalf("push: %+v", p)
	}
	if code, _ := e.raw(t, "GET", "/events/"+ev, mTok, nil); code != 404 {
		t.Fatalf("deleted detail: %d", code)
	}
}
