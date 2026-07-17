package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ── SMTP settings ───────────────────────────────────────────────────────────────

// smtpRowID is fixed: there is one SMTP server, so there is one row. A settings table that
// CAN hold two rows eventually does, and then which one applies is decided by whatever
// ORDER BY somebody happened to write.
const smtpRowID = "smtp"

type SMTPSettings struct {
	Host        string
	Port        int
	Username    string
	PasswordEnc string // sealed; never leaves the server
	FromAddr    string
	FromName    string
	BaseURL     string // for the "Open in Daffa" link; Daffa cannot know its own public URL
	Enabled     bool
	UpdatedAt   time.Time
}

func (s *Store) SMTPSettings(ctx context.Context) (*SMTPSettings, error) {
	var v SMTPSettings
	var enabled int
	var updated string

	err := s.queryRow(ctx, `SELECT host, port, username, password_enc, from_addr, from_name,
        base_url, enabled, updated_at FROM smtp_settings WHERE id = ?`, smtpRowID).
		Scan(&v.Host, &v.Port, &v.Username, &v.PasswordEnc, &v.FromAddr, &v.FromName,
			&v.BaseURL, &enabled, &updated)

	if errors.Is(err, sql.ErrNoRows) {
		// Never configured. Return the zero value rather than ErrNotFound: "email is off"
		// is a normal state, not an exceptional one, and making every caller special-case
		// it would put the same three lines in five places.
		return &SMTPSettings{Port: 587, FromName: "Daffa"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading SMTP settings: %w", err)
	}

	v.Enabled = enabled != 0
	v.UpdatedAt = parseTS(updated)
	return &v, nil
}

// SaveSMTPSettings upserts the single row. An empty PasswordEnc means "keep the stored
// one" — so an edit form that does not resend the password cannot blank it by omission.
func (s *Store) SaveSMTPSettings(ctx context.Context, v *SMTPSettings) error {
	pw := v.PasswordEnc
	if pw == "" {
		existing, err := s.SMTPSettings(ctx)
		if err != nil {
			return err
		}
		pw = existing.PasswordEnc
	}

	_, err := s.exec(ctx, `INSERT INTO smtp_settings
        (id, host, port, username, password_enc, from_addr, from_name, base_url, enabled, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT (id) DO UPDATE SET
            host = excluded.host, port = excluded.port, username = excluded.username,
            password_enc = excluded.password_enc, from_addr = excluded.from_addr,
            from_name = excluded.from_name, base_url = excluded.base_url,
            enabled = excluded.enabled, updated_at = excluded.updated_at`,
		smtpRowID, v.Host, v.Port, v.Username, pw, v.FromAddr, v.FromName, v.BaseURL,
		boolInt(v.Enabled), ts(now()))
	if err != nil {
		return fmt.Errorf("store: saving SMTP settings: %w", err)
	}
	v.PasswordEnc = pw
	return nil
}

// ── channels ────────────────────────────────────────────────────────────────────

// ChannelKinds are the non-email destinations. Each shapes the payload differently — see
// notify.RenderChannel — but they share a table because a channel is, in the end, a sealed URL
// you POST to.
var ChannelKinds = map[string]bool{"slack": true, "discord": true, "webhook": true}

// NotificationChannel is a chat/webhook destination. URLEnc is sealed and never returned to a
// client; URL is only ever populated inside the server, at send time, to actually POST.
type NotificationChannel struct {
	ID        string
	Kind      string
	Name      string
	URLEnc    string // sealed at rest; write-only over the wire
	Enabled   bool
	CreatedAt time.Time
}

func (s *Store) ListChannels(ctx context.Context) ([]NotificationChannel, error) {
	rows, err := s.query(ctx, `SELECT id, kind, name, enabled, created_at
        FROM notification_channels ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing notification channels: %w", err)
	}
	defer rows.Close()

	var out []NotificationChannel
	for rows.Next() {
		var c NotificationChannel
		var enabled int
		var created string
		if err := rows.Scan(&c.ID, &c.Kind, &c.Name, &enabled, &created); err != nil {
			return nil, err
		}
		c.Enabled = enabled != 0
		c.CreatedAt = parseTS(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CreateChannel(ctx context.Context, c *NotificationChannel) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	if !ChannelKinds[c.Kind] {
		return fmt.Errorf("store: %q is not a channel kind", c.Kind)
	}
	if c.Name == "" || c.URLEnc == "" {
		return errors.New("store: a channel needs a name and a URL")
	}
	_, err := s.exec(ctx, `INSERT INTO notification_channels (id, kind, name, url_enc, enabled, created_at)
        VALUES (?, ?, ?, ?, ?, ?)`, c.ID, c.Kind, c.Name, c.URLEnc, boolInt(c.Enabled), ts(now()))
	if err != nil {
		return fmt.Errorf("store: creating notification channel: %w", err)
	}
	return nil
}

// ChannelByID returns the channel including its sealed URL. Used by the worker to deliver, so it
// is the one place URLEnc is read back — and it is read back on the SERVER, never over the wire.
func (s *Store) ChannelByID(ctx context.Context, id string) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled int
	var created string
	err := s.queryRow(ctx, `SELECT id, kind, name, url_enc, enabled, created_at
        FROM notification_channels WHERE id = ?`, id).
		Scan(&c.ID, &c.Kind, &c.Name, &c.URLEnc, &enabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading notification channel: %w", err)
	}
	c.Enabled = enabled != 0
	c.CreatedAt = parseTS(created)
	return &c, nil
}

func (s *Store) DeleteChannel(ctx context.Context, id string) error {
	// The rules that route to it go too. The schema declares ON DELETE CASCADE, but SQLite only
	// enforces it when foreign_keys is on for the connection — so delete them here as well, and
	// the behaviour is the same whichever database is underneath.
	if _, err := s.exec(ctx, `DELETE FROM notification_rules WHERE channel_id = ?`, id); err != nil {
		return fmt.Errorf("store: detaching rules from channel: %w", err)
	}
	res, err := s.exec(ctx, `DELETE FROM notification_channels WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: deleting notification channel: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ── rules ───────────────────────────────────────────────────────────────────────

// NotificationRule is an event and one recipient. Exactly one of RoleID / Address / ChannelID
// is set.
type NotificationRule struct {
	ID        string
	Event     string
	RoleID    string
	Address   string
	ChannelID string

	RoleName    string // joined, for display
	ChannelName string
	ChannelKind string
}

func (s *Store) ListNotificationRules(ctx context.Context) ([]NotificationRule, error) {
	rows, err := s.query(ctx, `SELECT n.id, n.event, COALESCE(n.role_id, ''), n.address,
            COALESCE(n.channel_id, ''), COALESCE(r.name, ''),
            COALESCE(c.name, ''), COALESCE(c.kind, '')
        FROM notification_rules n
        LEFT JOIN roles r ON r.id = n.role_id
        LEFT JOIN notification_channels c ON c.id = n.channel_id
        ORDER BY n.event, r.name, c.name, n.address`)
	if err != nil {
		return nil, fmt.Errorf("store: listing notification rules: %w", err)
	}
	defer rows.Close()

	var out []NotificationRule
	for rows.Next() {
		var n NotificationRule
		if err := rows.Scan(&n.ID, &n.Event, &n.RoleID, &n.Address, &n.ChannelID,
			&n.RoleName, &n.ChannelName, &n.ChannelKind); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) CreateNotificationRule(ctx context.Context, n *NotificationRule) error {
	if n.ID == "" {
		n.ID = NewID()
	}
	// Exactly one target. Counting them is clearer than the old XOR once there are three, and it
	// is the invariant the resolver relies on — a rule with two targets would page twice.
	set := 0
	for _, v := range []string{n.RoleID, n.Address, n.ChannelID} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return errors.New("store: a notification rule needs exactly one of a role, an address, or a channel")
	}
	_, err := s.exec(ctx, `INSERT INTO notification_rules (id, event, role_id, address, channel_id, created_at)
        VALUES (?, ?, ?, ?, ?, ?)`, n.ID, n.Event, nullStr(n.RoleID), n.Address, nullStr(n.ChannelID), ts(now()))
	if err != nil {
		return fmt.Errorf("store: creating notification rule: %w", err)
	}
	return nil
}

// ChannelsFor returns the enabled channels an event routes to. Unlike RecipientsFor this needs no
// host scoping: a channel is a fixed destination, not a person whose standing depends on where the
// event happened.
func (s *Store) ChannelsFor(ctx context.Context, event string) ([]NotificationChannel, error) {
	rows, err := s.query(ctx, `SELECT DISTINCT c.id, c.kind, c.name, c.url_enc
        FROM notification_rules n
        JOIN notification_channels c ON c.id = n.channel_id
        WHERE n.event = ? AND c.enabled = 1`, event)
	if err != nil {
		return nil, fmt.Errorf("store: resolving channels: %w", err)
	}
	defer rows.Close()

	var out []NotificationChannel
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Kind, &c.Name, &c.URLEnc); err != nil {
			return nil, err
		}
		c.Enabled = true
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteNotificationRule(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM notification_rules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: deleting notification rule: %w", err)
	}
	return nil
}

// RecipientsFor resolves who should be told about an event on a host.
//
// Role recipients are resolved HERE, at send time, rather than expanded into a stored list
// when the rule was written — so the people who get paged track role membership by
// themselves, and nobody has to remember to update a distribution list when somebody joins.
//
// And the resolution is SCOPED: a role recipient yields only the users who hold that role at
// a scope covering this host. An Operator scoped to staging is not paged about production.
// That is the first place scoping actually pays for itself, and it is the difference between
// a notification people keep switched on and one they filter to a folder.
//
// envID may be "" for a fleet-level event (break-glass), in which case only GLOBAL holders
// of the role are notified — a staging operator has no standing to hear about it.
func (s *Store) RecipientsFor(ctx context.Context, event, envID string) ([]string, error) {
	rules, err := s.query(ctx,
		`SELECT COALESCE(role_id, ''), address FROM notification_rules WHERE event = ?`, event)
	if err != nil {
		return nil, fmt.Errorf("store: resolving recipients: %w", err)
	}
	defer rules.Close()

	seen := map[string]bool{}
	var out []string
	var roleIDs []string

	for rules.Next() {
		var roleID, address string
		if err := rules.Scan(&roleID, &address); err != nil {
			return nil, err
		}
		if address != "" {
			if !seen[address] {
				seen[address] = true
				out = append(out, address)
			}
			continue
		}
		if roleID != "" {
			roleIDs = append(roleIDs, roleID)
		}
	}
	if err := rules.Err(); err != nil {
		return nil, err
	}
	rules.Close()

	for _, roleID := range roleIDs {
		emails, err := s.roleMemberEmails(ctx, roleID, envID)
		if err != nil {
			return nil, err
		}
		for _, e := range emails {
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// roleMemberEmails finds the enabled users holding a role at a scope that covers envID.
//
// Disabled users are excluded: somebody who cannot sign in cannot act on the alert, and
// mailing a departed colleague's address for a year is its own small failure.
func (s *Store) roleMemberEmails(ctx context.Context, roleID, envID string) ([]string, error) {
	q := `SELECT DISTINCT u.email
        FROM role_members rm
        JOIN users u ON u.id = rm.user_id
        WHERE rm.role_id = ? AND u.disabled = 0 AND u.email <> ''
          AND (rm.scope_kind = 'global'`
	args := []any{roleID}

	if envID != "" {
		q += ` OR (rm.scope_kind = 'env' AND rm.scope_id = ?)`
		args = append(args, envID)
	}
	q += `)`

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: resolving role recipients: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ── outbox ──────────────────────────────────────────────────────────────────────

type OutboxMessage struct {
	ID        string
	Event     string
	Kind      string // email (default) | slack | discord | webhook
	To        string // email address, for an email message
	ChannelID string // the channel to POST to, for a channel message
	Subject   string
	HTML      string
	Text      string // the email text part, or the channel's JSON payload
	Status    string
	Attempts  int
	NextTryAt time.Time
	LastError string
	CreatedAt time.Time
}

// Enqueue adds a message. One row per recipient, so one bad address cannot take the others
// down with it — and a retry retries only the one that failed.
func (s *Store) Enqueue(ctx context.Context, m *OutboxMessage) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.Kind == "" {
		m.Kind = "email"
	}
	_, err := s.exec(ctx, `INSERT INTO notification_outbox
        (id, event, kind, to_addr, channel_id, subject, body_html, body_text, status, attempts, next_try_at, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0, ?, ?)`,
		m.ID, m.Event, m.Kind, m.To, m.ChannelID, m.Subject, m.HTML, m.Text, ts(now()), ts(now()))
	if err != nil {
		return fmt.Errorf("store: enqueuing a notification: %w", err)
	}
	return nil
}

// DueMessages returns what the worker should try now.
func (s *Store) DueMessages(ctx context.Context, limit int) ([]*OutboxMessage, error) {
	rows, err := s.query(ctx, `SELECT id, event, kind, to_addr, channel_id, subject, body_html, body_text,
            status, attempts, next_try_at, last_error, created_at
        FROM notification_outbox
        WHERE status = 'pending' AND next_try_at <= ?
        ORDER BY next_try_at LIMIT ?`, ts(now()), limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading the outbox: %w", err)
	}
	defer rows.Close()

	var out []*OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		var next, created string
		if err := rows.Scan(&m.ID, &m.Event, &m.Kind, &m.To, &m.ChannelID, &m.Subject, &m.HTML, &m.Text,
			&m.Status, &m.Attempts, &next, &m.LastError, &created); err != nil {
			return nil, err
		}
		m.NextTryAt, m.CreatedAt = parseTS(next), parseTS(created)
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (s *Store) MarkSent(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `UPDATE notification_outbox
        SET status = 'sent', attempts = attempts + 1, sent_at = ?, last_error = ''
        WHERE id = ?`, ts(now()), id)
	return err
}

// MarkFailed records an attempt. If there are retries left the message stays pending with a
// later next_try_at; otherwise it is marked failed and LEFT IN PLACE — a dead letter you can
// look at, not a message that quietly evaporated.
func (s *Store) MarkFailed(ctx context.Context, id string, attempts int, retryAt time.Time, cause string) error {
	status := "pending"
	if retryAt.IsZero() {
		status = "failed"
		retryAt = now()
	}
	_, err := s.exec(ctx, `UPDATE notification_outbox
        SET status = ?, attempts = ?, next_try_at = ?, last_error = ?
        WHERE id = ?`, status, attempts, ts(retryAt), cause, id)
	return err
}

// FailedMessages is what the settings page shows: the ones that gave up. Silence about a
// failed alert is the worst possible outcome — you would believe nothing had gone wrong.
func (s *Store) FailedMessages(ctx context.Context, limit int) ([]*OutboxMessage, error) {
	// A channel message has no to_addr — its destination is the channel, so fall back to the
	// channel name (still joinable even after the channel is deleted? no — so COALESCE leaves the
	// kind, which at least says "a Slack message failed" rather than a blank cell).
	rows, err := s.query(ctx, `SELECT o.id, o.event, o.kind,
            CASE WHEN o.to_addr <> '' THEN o.to_addr ELSE COALESCE(c.name, o.kind) END,
            o.subject, o.attempts, o.last_error, o.created_at
        FROM notification_outbox o
        LEFT JOIN notification_channels c ON c.id = o.channel_id
        WHERE o.status = 'failed'
        ORDER BY o.created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading failed notifications: %w", err)
	}
	defer rows.Close()

	var out []*OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		var created string
		if err := rows.Scan(&m.ID, &m.Event, &m.Kind, &m.To, &m.Subject, &m.Attempts, &m.LastError, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTS(created)
		out = append(out, &m)
	}
	return out, rows.Err()
}
