// Package mailer sends email over SMTP.
//
// It is small on purpose: one transport, one message shape, no attachments, no CC. Daffa
// sends alerts, not correspondence.
//
// It deliberately imports nothing outside the standard library, which keeps it trivial to
// audit and to lift. If a second consumer ever appears, this package is the thing to extract.
package mailer

import "context"

// Message is one email to one person.
//
// To is a single address, not a list. Fanning out to several recipients means several
// messages, which is what the outbox does — one row each, so one bad address cannot take
// the others down with it.
type Message struct {
	From     string
	FromName string // optional display name; the SMTP envelope keeps the bare address
	To       string
	Subject  string
	HTML     string
	Text     string // the plain-text alternative; never omit it
}

// Transport is how a message leaves the building.
type Transport interface {
	Send(ctx context.Context, m Message) error
}
