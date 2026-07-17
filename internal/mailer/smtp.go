package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SMTP is the transport. Port 465 gets implicit TLS; anything else attempts STARTTLS.
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
}

func (s *SMTP) auth() smtp.Auth {
	if s.Username == "" {
		return nil
	}
	return smtp.PlainAuth("", s.Username, s.Password, s.Host)
}

func (s *SMTP) Send(ctx context.Context, m Message) error {
	if s.Host == "" {
		return errors.New("mailer: no SMTP server is configured")
	}
	if m.To == "" {
		return errors.New("mailer: no recipient")
	}
	if m.From == "" {
		return errors.New("mailer: no sender — set a From address in the notification settings")
	}

	body := buildMIME(m)
	addr := fmt.Sprintf("%s:%d", s.Host, s.Port)

	if s.Port == 465 {
		return s.sendTLS(ctx, addr, m.From, m.To, body)
	}
	return s.sendSTARTTLS(ctx, addr, m.From, m.To, body)
}

func (s *SMTP) sendSTARTTLS(ctx context.Context, addr, from, to string, body []byte) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mailer: dialling %s: %w", addr, err)
	}
	deadlineConn(ctx, conn)

	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mailer: smtp handshake: %w", err)
	}
	defer c.Close()

	// Opportunistic: a server that does not advertise STARTTLS (a local relay, a test
	// server on a private network) still gets the mail. On the public internet it always
	// will, and then we always use it.
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
			return fmt.Errorf("mailer: starttls: %w", err)
		}
	}
	return s.deliver(c, from, to, body)
}

func (s *SMTP) sendTLS(ctx context.Context, addr, from, to string, body []byte) error {
	// tls.Dialer.DialContext honours ctx for the connect; tls.DialWithDialer did NOT — it
	// ignored the context entirely, so a firewall silently dropping packets after the TCP
	// handshake could hang this dial forever, and with it the whole single-threaded outbox.
	d := tls.Dialer{Config: &tls.Config{ServerName: s.Host}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mailer: TLS dial to %s: %w", addr, err)
	}
	deadlineConn(ctx, conn)

	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mailer: smtp handshake: %w", err)
	}
	defer c.Close()

	return s.deliver(c, from, to, body)
}

// deadlineConn pins an absolute deadline over the ENTIRE SMTP conversation — connect is
// context-aware, but AUTH/MAIL/RCPT/DATA/QUIT are not, so without this a server that accepts the
// TCP connection and then stops responding would block a send indefinitely (OS TCP timeout is
// minutes), stalling the serial outbox behind it. The worker gives ctx a ~30s deadline; honour
// it, and fall back to a fixed bound if ctx somehow carries none.
func deadlineConn(ctx context.Context, conn net.Conn) {
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(30 * time.Second)
	}
	_ = conn.SetDeadline(dl)
}

func (s *SMTP) deliver(c *smtp.Client, from, to string, body []byte) error {
	if a := s.auth(); a != nil {
		// A username is configured, so authenticate — but only if the server will take it.
		//
		// Plenty of servers legitimately want none: a relay on localhost, a company MTA that
		// authenticates by IP. Attempting AUTH against one of those fails with "502 command
		// not implemented", which tells an administrator nothing about what to do. Say it
		// plainly instead: the username is the thing that is wrong.
		if ok, _ := c.Extension("AUTH"); !ok {
			return fmt.Errorf("mailer: %s does not accept authentication, but a username is "+
				"configured. Clear the username and password if this server does not need them", s.Host)
		}
		if err := c.Auth(a); err != nil {
			return fmt.Errorf("mailer: authenticating to %s: %w", s.Host, err)
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("mailer: MAIL FROM %s: %w", from, err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("mailer: RCPT TO %s: %w", to, err)
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mailer: DATA: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		w.Close()
		return fmt.Errorf("mailer: writing the message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: closing the message: %w", err)
	}
	return c.Quit()
}

// buildMIME writes a multipart/alternative message: plain text first, HTML second, which is
// the order that tells a client "prefer the HTML if you can render it".
func buildMIME(m Message) []byte {
	const boundary = "==daffa-boundary=="

	var b strings.Builder
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "From: %s\r\n", addressHeader(m.FromName, m.From))
	fmt.Fprintf(&b, "To: %s\r\n", m.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", m.Subject)
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n", boundary)
	b.WriteString("\r\n")

	// Both parts are quoted-printable encoded, and the TEXT one is not optional.
	//
	// Without it, any '=' in the body — every query string, every link with a parameter —
	// is read by the receiving client as the start of a quoted-printable escape, and the
	// URL arrives mangled. The HTML part still looks fine, so the bug presents as "some
	// people's links don't work", which is a miserable afternoon. This cost the platform
	// exactly that afternoon; the fix travels with the code.
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	b.WriteString(qp(m.Text))
	b.WriteString("\r\n")

	if m.HTML != "" {
		fmt.Fprintf(&b, "--%s\r\n", boundary)
		b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		b.WriteString(qp(m.HTML))
		b.WriteString("\r\n")
	}

	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []byte(b.String())
}

// addressHeader renders `"Daffa" <ops@example.com>`, or the bare address when there is no
// display name. The SMTP envelope always carries the bare address; only the header gets
// the name.
func addressHeader(name, addr string) string {
	if name = strings.TrimSpace(name); name == "" {
		return addr
	}
	return fmt.Sprintf("%q <%s>", name, addr)
}

func qp(s string) string {
	var b strings.Builder
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return b.String()
}
