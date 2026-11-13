// Package mailer sends the platform's transactional emails (new-device
// login alerts now; invites and receipts later) through the email provider.
package mailer

import (
	"context"
	"log"
)

// Message is one plain-text email.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Mailer delivers a message. Handlers send in the background, so an email
// failure never fails the request that triggered it.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// Log is the Mailer used until an email provider is wired in (EMAIL_API_KEY
// unset): it logs the recipient domain and subject, never the body.
type Log struct{}

// Send implements Mailer.
func (Log) Send(_ context.Context, m Message) error {
	domain := m.To
	for i := len(m.To) - 1; i >= 0; i-- {
		if m.To[i] == '@' {
			domain = m.To[i+1:]
			break
		}
	}
	log.Printf("email (no provider configured): to=*@%s subject=%q", domain, m.Subject)
	return nil
}
