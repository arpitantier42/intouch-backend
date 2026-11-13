package server

import (
	"fmt"
	"net/url"
	"sort"
	"sync"
	"testing"
	"time"
)

// TestSendDeliveryLoad is the server-side half of the Realtime latency check:
// several senders post concurrently into one group while the recipient polls
// the catch-up path (after=last seen). Every message must arrive (no gaps on
// reconnect) and the send→visible p95 must stay under 1 s. The Realtime
// fan-out itself can only be measured against a deployed Supabase project.
func TestSendDeliveryLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	e, _ := newMsgEnv(t)
	owner, oTok := admUser(t, e, "owner@example.com")
	reader, rTok := admUser(t, e, "reader@example.com")
	cid, _ := admCommunity(t, e, owner, "Load", "private")
	chatJoin(t, e, cid, reader)
	const senders, perSender = 5, 20
	toks := []string{oTok}
	ids := reader
	for i := 1; i < senders; i++ {
		uid, tok := admUser(t, e, fmt.Sprintf("s%d@example.com", i))
		chatJoin(t, e, cid, uid)
		toks = append(toks, tok)
		ids += "," + uid
	}
	_, g := e.raw(t, "POST", "/communities/"+cid+"/threads/groups?member_ids="+ids, oTok, map[string]string{"name": "Load"})
	gid := field(g, "id").(string)

	var mu sync.Mutex
	sentAt := map[string]time.Time{}
	var wg sync.WaitGroup
	for s := 0; s < senders; s++ {
		wg.Add(1)
		go func(tok string, s int) {
			defer wg.Done()
			for i := 0; i < perSender; i++ {
				start := time.Now()
				code, out := send(t, e, gid, cid, tok, text(fmt.Sprintf("m%d-%d", s, i)))
				if code != 201 {
					t.Errorf("send: %d %v", code, out)
					return
				}
				mu.Lock()
				sentAt[field(out, "id").(string)] = start
				mu.Unlock()
			}
		}(toks[s], s)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	seen := map[string]time.Duration{}
	last := ""
	poll := func() {
		path := "/threads/" + gid + "/messages?limit=200"
		if last != "" {
			path += "&after=" + url.QueryEscape(last)
		}
		_, list := e.raw(t, "GET", path, rTok, nil)
		now := time.Now()
		for _, m := range list.([]any) {
			id := field(m, "id").(string)
			if _, ok := seen[id]; ok {
				continue
			}
			mu.Lock()
			st, ok := sentAt[id]
			mu.Unlock()
			if !ok {
				st = now
			}
			seen[id] = now.Sub(st)
			if c := field(m, "created_at").(string); c > last {
				last = c
			}
		}
	}
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			poll()
			time.Sleep(20 * time.Millisecond)
		}
	}
	// Commits can land with an earlier created_at than one already seen;
	// a final full read (what a reconnecting client does from its
	// last_read) must contain everything.
	last = ""
	poll()
	if len(seen) != senders*perSender {
		t.Fatalf("delivered %d of %d", len(seen), senders*perSender)
	}
	lat := make([]time.Duration, 0, len(seen))
	for _, d := range seen {
		lat = append(lat, d)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p95 := lat[len(lat)*95/100]
	t.Logf("send→visible p95 = %v over %d messages", p95, len(lat))
	if p95 > time.Second {
		t.Fatalf("p95 %v ≥ 1s", p95)
	}
}
