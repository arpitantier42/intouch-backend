// Package push delivers mobile push notifications (APNs on iOS, FCM on
// Android) through the push provider. Handlers send in the background, so a
// push failure never fails the request that triggered it.
package push

import (
	"context"
	"log"
)

// Notification is one alert for a set of users. Data carries referenced ids
// only (e.g. thread_id, message_id) so the app can open the right screen.
type Notification struct {
	UserIDs []string
	Title   string
	Body    string
	Data    map[string]string
}

// Sender delivers a notification to every device of each user.
type Sender interface {
	Send(ctx context.Context, n Notification) error
}

// Log is the Sender used until a push provider is wired in: it logs the
// recipient count and data keys, never the text.
type Log struct{}

// Send implements Sender.
func (Log) Send(_ context.Context, n Notification) error {
	log.Printf("push (no provider configured): recipients=%d data=%v", len(n.UserIDs), keys(n.Data))
	return nil
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
