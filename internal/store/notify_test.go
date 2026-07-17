package store

import (
	"context"
	"testing"
	"time"

	"github.com/Mnshahawy/daffa/internal/caps"
)

// A role recipient is resolved at SEND time, and it honours scope.
//
// This is where scoping actually pays for itself: an Operator scoped to staging is not paged
// about a production deploy. Without it, the first thing anybody does with a scoped role is
// mute the notifications — and then the one that mattered goes unread with the rest.
func TestRecipientsHonourScope(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		prod, staging := twoHosts(t, s)

		op, err := s.RoleByName(ctx, "Operator")
		if err != nil {
			t.Fatal(err)
		}
		admin, err := s.AdminRole(ctx)
		if err != nil {
			t.Fatal(err)
		}

		// Sara operates staging. Raj operates prod. Root is an administrator everywhere.
		for _, u := range []struct {
			name, email string
			role        string
			scope       Scope
		}{
			{"sara", "sara@example.com", op.ID, OnEnv(staging.ID)},
			{"raj", "raj@example.com", op.ID, OnEnv(prod.ID)},
			{"root", "root@example.com", admin.ID, Global()},
		} {
			user := &User{Kind: "local", Username: u.name, Email: u.email}
			if err := s.CreateUser(ctx, user); err != nil {
				t.Fatal(err)
			}
			if err := s.GrantRole(ctx, user.ID, u.role, SourceLocal, u.scope); err != nil {
				t.Fatal(err)
			}
		}

		// One rule: tell every Operator when a deploy fails. Plus a pager, which is not a
		// Daffa user at all and therefore has no scope.
		for _, r := range []*NotificationRule{
			{Event: "deploy.failed", RoleID: op.ID},
			{Event: "deploy.failed", Address: "oncall@example.com"},
		} {
			if err := s.CreateNotificationRule(ctx, r); err != nil {
				t.Fatal(err)
			}
		}

		// A deploy fails on PROD.
		got, err := s.RecipientsFor(ctx, "deploy.failed", prod.ID)
		if err != nil {
			t.Fatal(err)
		}
		set := map[string]bool{}
		for _, e := range got {
			set[e] = true
		}

		if !set["raj@example.com"] {
			t.Error("the operator who runs prod was not told about a prod failure")
		}
		if set["sara@example.com"] {
			t.Error("the operator scoped to STAGING was paged about a PROD failure — " +
				"scope is not being honoured, and this is the notification people mute")
		}
		if !set["oncall@example.com"] {
			t.Error("the literal pager address was not told — a rule with an explicit " +
				"address has no scope to filter on and must always fire")
		}

		// And the same failure on staging reaches Sara, not Raj.
		got, err = s.RecipientsFor(ctx, "deploy.failed", staging.ID)
		if err != nil {
			t.Fatal(err)
		}
		set = map[string]bool{}
		for _, e := range got {
			set[e] = true
		}
		if !set["sara@example.com"] || set["raj@example.com"] {
			t.Errorf("a staging failure went to %v; want sara, not raj", got)
		}
	})
}

// A fleet-level event (break-glass) has no host. Only GLOBAL holders of a role hear about
// it — an operator scoped to one host has no standing to be told that somebody became an
// administrator.
func TestFleetLevelEventsGoToGlobalHoldersOnly(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		_, staging := twoHosts(t, s)

		op, _ := s.RoleByName(ctx, "Operator")

		scoped := &User{Kind: "local", Username: "sara", Email: "sara@example.com"}
		global := &User{Kind: "local", Username: "gil", Email: "gil@example.com"}
		if err := s.CreateUser(ctx, scoped); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateUser(ctx, global); err != nil {
			t.Fatal(err)
		}
		if err := s.GrantRole(ctx, scoped.ID, op.ID, SourceLocal, OnEnv(staging.ID)); err != nil {
			t.Fatal(err)
		}
		if err := s.GrantRole(ctx, global.ID, op.ID, SourceLocal, Global()); err != nil {
			t.Fatal(err)
		}

		if err := s.CreateNotificationRule(ctx,
			&NotificationRule{Event: "breakglass.used", RoleID: op.ID}); err != nil {
			t.Fatal(err)
		}

		got, err := s.RecipientsFor(ctx, "breakglass.used", "") // no host: fleet-level
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "gil@example.com" {
			t.Errorf("a fleet-level event went to %v; want only the global holder", got)
		}
	})
}

// A disabled user cannot act on an alert, and mailing a departed colleague for a year is its
// own small failure.
func TestDisabledUsersAreNotNotified(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		op, _ := s.RoleByName(ctx, "Operator")
		u := &User{Kind: "local", Username: "gone", Email: "gone@example.com", Disabled: true}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if err := s.GrantRole(ctx, u.ID, op.ID, SourceLocal, Global()); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateNotificationRule(ctx,
			&NotificationRule{Event: "deploy.failed", RoleID: op.ID}); err != nil {
			t.Fatal(err)
		}

		got, err := s.RecipientsFor(ctx, "deploy.failed", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("a disabled user was notified: %v", got)
		}
	})
}

// The SMTP password is sealed and must never be blanked by an edit form that did not resend
// it — the same rule the OIDC client secret follows.
func TestSavingSMTPKeepsTheStoredPassword(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		if err := s.SaveSMTPSettings(ctx, &SMTPSettings{
			Host: "smtp.example.com", Port: 587, Username: "u",
			PasswordEnc: "sealed-v1", FromAddr: "a@b", Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}

		// An edit that changes the port and does NOT resend the password.
		if err := s.SaveSMTPSettings(ctx, &SMTPSettings{
			Host: "smtp.example.com", Port: 465, Username: "u",
			PasswordEnc: "", FromAddr: "a@b", Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}

		got, err := s.SMTPSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.PasswordEnc != "sealed-v1" {
			t.Fatalf("the stored password was blanked by an edit that did not resend it: %q", got.PasswordEnc)
		}
		if got.Port != 465 {
			t.Errorf("the edit did not take: port = %d", got.Port)
		}
	})
}

// Never configured is a normal state, not an error. Every caller would otherwise carry the
// same three lines of special-casing.
func TestUnconfiguredSMTPIsNotAnError(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		got, err := s.SMTPSettings(context.Background())
		if err != nil {
			t.Fatalf("reading unconfigured SMTP settings: %v", err)
		}
		if got.Enabled || got.Host != "" {
			t.Errorf("a fresh store thinks email is configured: %+v", got)
		}
		if got.Port != 587 {
			t.Errorf("the default port is %d; want 587", got.Port)
		}
	})
}

// The outbox is the whole reason a slow mail server cannot hold a database lock.
func TestOutboxRetriesThenGivesUpVisibly(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		m := &OutboxMessage{Event: "backup.failed", To: "a@b", Subject: "x", Text: "y"}
		if err := s.Enqueue(ctx, m); err != nil {
			t.Fatal(err)
		}

		due, err := s.DueMessages(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 {
			t.Fatalf("a freshly enqueued message is not due: %d", len(due))
		}

		// A failure with retries left: still pending, but not due yet.
		if err := s.MarkFailed(ctx, m.ID, 1, time.Now().Add(time.Hour), "connection refused"); err != nil {
			t.Fatal(err)
		}
		due, _ = s.DueMessages(ctx, 10)
		if len(due) != 0 {
			t.Error("a message scheduled an hour out came back as due immediately")
		}

		// Giving up: marked failed, and STILL THERE. A failed alert that vanishes is the
		// worst outcome the system has — you would believe nothing had gone wrong.
		if err := s.MarkFailed(ctx, m.ID, 6, time.Time{}, "connection refused"); err != nil {
			t.Fatal(err)
		}
		failed, err := s.FailedMessages(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(failed) != 1 || failed[0].LastError != "connection refused" {
			t.Fatalf("a message that gave up is not in the dead-letter list: %+v", failed)
		}

		// And a sent one leaves the queue.
		m2 := &OutboxMessage{Event: "backup.failed", To: "c@d", Subject: "x", Text: "y"}
		if err := s.Enqueue(ctx, m2); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkSent(ctx, m2.ID); err != nil {
			t.Fatal(err)
		}
		due, _ = s.DueMessages(ctx, 10)
		for _, d := range due {
			if d.ID == m2.ID {
				t.Error("a sent message is still due")
			}
		}
	})
}

// The seeded roles must still be intact — the notify tests lean on them.
func TestNotifyRuleNeedsExactlyOneRecipient(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		op, _ := s.RoleByName(ctx, "Operator")

		if err := s.CreateNotificationRule(ctx, &NotificationRule{Event: "deploy.failed"}); err == nil {
			t.Error("a rule with neither a role nor an address was accepted — it would notify nobody")
		}
		if err := s.CreateNotificationRule(ctx, &NotificationRule{
			Event: "deploy.failed", RoleID: op.ID, Address: "a@b",
		}); err == nil {
			t.Error("a rule with both a role and an address was accepted — which wins?")
		}
	})
}

// Channels are the whole point of the feature: a route from an event to a chat webhook, resolved
// at send time, that does not depend on email being configured at all.
func TestChannelRoutingResolvesForAnEvent(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		ch := &NotificationChannel{Kind: "slack", Name: "ops", URLEnc: "sealed-url", Enabled: true}
		if err := s.CreateChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}

		// A rule targeting the channel. Exactly-one-target must accept a channel as that one.
		if err := s.CreateNotificationRule(ctx, &NotificationRule{
			Event: "deploy.failed", ChannelID: ch.ID,
		}); err != nil {
			t.Fatalf("a channel rule was rejected: %v", err)
		}
		// And still reject two targets, now that there are three kinds.
		if err := s.CreateNotificationRule(ctx, &NotificationRule{
			Event: "deploy.failed", Address: "a@b", ChannelID: ch.ID,
		}); err == nil {
			t.Error("a rule with both an address and a channel was accepted")
		}

		got, err := s.ChannelsFor(ctx, "deploy.failed")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != ch.ID || got[0].URLEnc != "sealed-url" {
			t.Fatalf("the event did not resolve to its channel with the sealed URL: %+v", got)
		}

		// A disabled channel routes to nobody — the off switch has to actually be off.
		if none, _ := s.ChannelsFor(ctx, "backup.failed"); len(none) != 0 {
			t.Errorf("an event with no channel rule resolved to %d channels", len(none))
		}

		// Deleting the channel takes its routing rule with it, so the event stops resolving —
		// otherwise a deleted channel leaves a rule that silently drops every alert.
		if err := s.DeleteChannel(ctx, ch.ID); err != nil {
			t.Fatal(err)
		}
		if after, _ := s.ChannelsFor(ctx, "deploy.failed"); len(after) != 0 {
			t.Errorf("deleting a channel left %d dangling routes behind", len(after))
		}
		rules, _ := s.ListNotificationRules(ctx)
		for _, r := range rules {
			if r.ChannelID == ch.ID {
				t.Error("a rule still points at the deleted channel")
			}
		}
	})
}

// A channel message round-trips through the outbox carrying its kind and channel, and the JSON
// payload rides in the text column.
func TestOutboxCarriesChannelMessages(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		m := &OutboxMessage{
			Event: "deploy.failed", Kind: "slack", ChannelID: "chan_1",
			Subject: "Deploy failed", Text: `{"text":"boom"}`,
		}
		if err := s.Enqueue(ctx, m); err != nil {
			t.Fatal(err)
		}
		due, err := s.DueMessages(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 {
			t.Fatalf("channel message not due: %d", len(due))
		}
		got := due[0]
		if got.Kind != "slack" || got.ChannelID != "chan_1" || got.Text != `{"text":"boom"}` {
			t.Errorf("channel fields did not survive the round trip: %+v", got)
		}
		// An email message still defaults its kind, so old callers that never set it keep working.
		e := &OutboxMessage{Event: "deploy.failed", To: "a@b", Subject: "x", Text: "y"}
		if err := s.Enqueue(ctx, e); err != nil {
			t.Fatal(err)
		}
		due, _ = s.DueMessages(ctx, 10)
		for _, d := range due {
			if d.ID == e.ID && d.Kind != "email" {
				t.Errorf("an email message defaulted to kind %q, not email", d.Kind)
			}
		}
	})
}

var _ = caps.Everything // keep the import honest if the assertions above change
