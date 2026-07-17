package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/mailer"
	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/store"
)

// ── SMTP settings ───────────────────────────────────────────────────────────────

type smtpView struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	FromAddr string `json:"from_addr"`
	FromName string `json:"from_name"`
	BaseURL  string `json:"base_url"`
	Enabled  bool   `json:"enabled"`

	// HasPassword, never the password. It is sealed with the master key and there is no
	// endpoint that reads it back, so there is nothing to leak.
	HasPassword bool `json:"has_password"`
}

func (s *Server) handleGetSMTP(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.SMTPSettings(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, smtpView{
		Host: cfg.Host, Port: cfg.Port, Username: cfg.Username,
		FromAddr: cfg.FromAddr, FromName: cfg.FromName, BaseURL: cfg.BaseURL,
		Enabled: cfg.Enabled, HasPassword: cfg.PasswordEnc != "",
	})
}

type smtpRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"` // "" ⇒ keep the stored one
	FromAddr string `json:"from_addr"`
	FromName string `json:"from_name"`
	BaseURL  string `json:"base_url"`
	Enabled  bool   `json:"enabled"`
}

func (s *Server) handleSaveSMTP(w http.ResponseWriter, r *http.Request) {
	var req smtpRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	req.Host = strings.TrimSpace(req.Host)
	req.FromAddr = strings.TrimSpace(req.FromAddr)
	req.BaseURL = strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")

	if req.Enabled {
		if req.Host == "" {
			httpx.BadRequest(w, r, "An SMTP server is required to send email.")
			return
		}
		if !strings.Contains(req.FromAddr, "@") {
			httpx.BadRequest(w, r, "A From address is required, and it has to look like an email address.")
			return
		}
	}
	if req.Port == 0 {
		req.Port = 587
	}
	if req.FromName == "" {
		req.FromName = "Daffa"
	}

	cfg := &store.SMTPSettings{
		Host: req.Host, Port: req.Port, Username: req.Username,
		FromAddr: req.FromAddr, FromName: req.FromName, BaseURL: req.BaseURL,
		Enabled: req.Enabled,
	}

	// An empty password means "leave it alone", so an edit form that does not resend it
	// cannot blank it by omission — the same rule the OIDC client secret follows.
	if req.Password != "" {
		sealed, err := s.sealer.Seal(req.Password)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		cfg.PasswordEnc = sealed
	}

	if err := s.store.SaveSMTPSettings(r.Context(), cfg); err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.auditNotify(r, "notify.smtp", req.Host)
	httpx.JSON(w, http.StatusOK, smtpView{
		Host: cfg.Host, Port: cfg.Port, Username: cfg.Username,
		FromAddr: cfg.FromAddr, FromName: cfg.FromName, BaseURL: cfg.BaseURL,
		Enabled: cfg.Enabled, HasPassword: cfg.PasswordEnc != "",
	})
}

// handleTestSMTP sends one real email, now, synchronously, to the person who asked.
//
// It bypasses the outbox on purpose: the whole point is to find out whether it works, and a
// queued message that fails silently four minutes later answers nothing. The error text
// comes straight back — a bad password, a wrong port and an unreachable host all look
// identical from the outside, and the SMTP server's own words are the fastest way through.
func (s *Server) handleTestSMTP(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	if u == nil || u.Email == "" {
		httpx.BadRequest(w, r,
			"Your account has no email address, so there is nowhere to send the test. Add one first.")
		return
	}

	transport, cfg, err := s.notify.Transport(r.Context())
	if err != nil {
		httpx.JSON(w, http.StatusOK, testResult{OK: false, Error: err.Error()})
		return
	}
	if transport == nil {
		httpx.JSON(w, http.StatusOK, testResult{
			OK:    false,
			Error: "Email is switched off, or no SMTP server is configured. Save the settings first.",
		})
		return
	}

	msg, err := notify.Preview(notify.BackupFailed)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	err = transport.Send(ctx, mailer.Message{
		From: cfg.FromAddr, FromName: cfg.FromName, To: u.Email,
		Subject: "[test] " + msg.Subject, HTML: msg.HTML, Text: msg.Text,
	})
	if err != nil {
		httpx.JSON(w, http.StatusOK, testResult{OK: false, Error: err.Error()})
		return
	}

	s.auditNotify(r, "notify.test", u.Email)
	httpx.JSON(w, http.StatusOK, testResult{
		OK:      true,
		Message: "Sent to " + u.Email + ". If it does not arrive, check the spam folder before the settings.",
	})
}

// ── channels (Slack / Discord / webhook) ────────────────────────────────────────

type channelView struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// No URL. Like an SMTP password it is sealed and write-only — a Slack webhook URL is a
	// bearer credential, and there is nothing to gain by handing it back.
}

func channelViews(cs []store.NotificationChannel) []channelView {
	out := make([]channelView, 0, len(cs))
	for _, c := range cs {
		out = append(out, channelView{ID: c.ID, Kind: c.Kind, Name: c.Name, Enabled: c.Enabled})
	}
	return out
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	cs, err := s.store.ListChannels(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, channelViews(cs))
}

type channelRequest struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// handleCreateChannel seals the URL and, like a storage target, PROVES it works before saving:
// it POSTs a real "connected" message. A chat webhook that 404s is not a configuration, it is a
// future silent failure — and unlike an email address, a webhook can be tested for free, so there
// is no excuse to save one untried. The confirmation message in the channel is a feature, not a
// side effect: it is how the person on the other end knows the wiring is done.
func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req channelRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)

	if !store.ChannelKinds[req.Kind] {
		httpx.BadRequest(w, r, "Choose a channel type: Slack, Discord, or a generic webhook.")
		return
	}
	if req.Name == "" {
		httpx.BadRequest(w, r, "Give the channel a name — it is what the routing rules refer to.")
		return
	}
	// https for Slack/Discord (they offer nothing else); http tolerated for a generic webhook to
	// an internal service, which is a legitimate thing to point this at. Anything without a scheme
	// is a paste error worth catching now.
	if !strings.HasPrefix(req.URL, "https://") && !strings.HasPrefix(req.URL, "http://") {
		httpx.BadRequest(w, r, "A channel needs an http(s):// webhook URL.")
		return
	}

	if err := s.postChannelTest(r.Context(), req.Kind, req.URL); err != nil {
		httpx.Fail(w, r, http.StatusBadRequest, "channel_unreachable", err.Error())
		return
	}

	sealed, err := s.sealer.Seal(req.URL)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	c := &store.NotificationChannel{Kind: req.Kind, Name: req.Name, URLEnc: sealed, Enabled: true}
	if err := s.store.CreateChannel(r.Context(), c); err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.auditNotify(r, "notify.channel.create", req.Name)
	httpx.JSON(w, http.StatusCreated, channelView{ID: c.ID, Kind: c.Kind, Name: c.Name, Enabled: c.Enabled})
}

// handleTestChannel re-sends the connected message to a saved channel, for when someone wants to
// confirm it still works without deleting and recreating it.
func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.ChannelByID(r.Context(), r.PathValue("id"))
	if err != nil {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_channel", "No such channel.")
		return
	}
	url, err := s.sealer.Open(c.URLEnc)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := s.postChannelTest(r.Context(), c.Kind, url); err != nil {
		httpx.JSON(w, http.StatusOK, testResult{OK: false, Error: err.Error()})
		return
	}
	httpx.JSON(w, http.StatusOK, testResult{OK: true, Message: "Posted to " + c.Name + "."})
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteChannel(r.Context(), r.PathValue("id")); err != nil {
		if err == store.ErrNotFound {
			httpx.Fail(w, r, http.StatusNotFound, "no_such_channel", "No such channel.")
			return
		}
		httpx.Error(w, r, err)
		return
	}
	s.auditNotify(r, "notify.channel.delete", r.PathValue("id"))
	httpx.JSON(w, http.StatusOK, okStatus)
}

// postChannelTest renders and delivers a sample message, synchronously, returning the provider's
// own error verbatim. It is the shared engine under both create-time verification and the test
// button, so "does this URL work?" is answered exactly one way.
func (s *Server) postChannelTest(ctx context.Context, kind, url string) error {
	d := notify.Data{
		Event:   notify.DeploySucceeded,
		Subject: "Daffa is connected",
		Title:   "Daffa is connected",
		Summary: "This channel will receive the notifications you route to it.",
		Failed:  false,
	}
	payload, err := notify.RenderChannel(kind, d)
	if err != nil {
		return err
	}
	postCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return notify.PostChannel(postCtx, http.DefaultClient, url, payload)
}

// ── events, rules, previews ─────────────────────────────────────────────────────

// notifyEventView is one notification the system can send, as the settings page lists them.
type notifyEventView struct {
	Event       string `json:"event"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Noisy       bool   `json:"noisy"`
}

func (s *Server) handleListNotifyEvents(w http.ResponseWriter, r *http.Request) {
	out := make([]notifyEventView, 0, len(notify.All))
	for _, d := range notify.All {
		out = append(out, notifyEventView{string(d.Event), d.Label, d.Description, d.Noisy})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// notifyPreviewResponse is a rendered notification: what the email's subject, HTML body and
// text alternative would say.
type notifyPreviewResponse struct {
	Subject string `json:"subject"`
	HTML    string `json:"html"`
	Text    string `json:"text"`
}

// handlePreviewNotification renders an event with plausible data. It is the only way to see
// what one of these looks like without breaking something first.
func (s *Server) handlePreviewNotification(w http.ResponseWriter, r *http.Request) {
	e := notify.Event(r.PathValue("event"))
	if !e.Valid() {
		httpx.Fail(w, r, http.StatusNotFound, "unknown_event", "No such notification.")
		return
	}
	msg, err := notify.Preview(e)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, notifyPreviewResponse{
		Subject: msg.Subject, HTML: msg.HTML, Text: msg.Text,
	})
}

func (s *Server) handleListNotifyRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.store.ListNotificationRules(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if rules == nil {
		rules = []store.NotificationRule{}
	}
	httpx.JSON(w, http.StatusOK, rules)
}

type ruleRequest struct {
	Event     string `json:"event"`
	RoleID    string `json:"role_id"`
	Address   string `json:"address"`
	ChannelID string `json:"channel_id"`
}

func (s *Server) handleCreateNotifyRule(w http.ResponseWriter, r *http.Request) {
	var req ruleRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Address = strings.TrimSpace(req.Address)

	if !notify.Event(req.Event).Valid() {
		httpx.BadRequest(w, r, "No such notification.")
		return
	}
	// Exactly one target of the three.
	targets := 0
	for _, v := range []string{req.RoleID, req.Address, req.ChannelID} {
		if v != "" {
			targets++
		}
	}
	if targets != 1 {
		httpx.BadRequest(w, r, "Choose one target: a role, an email address, or a channel.")
		return
	}
	if req.Address != "" && !strings.Contains(req.Address, "@") {
		httpx.BadRequest(w, r, "That does not look like an email address.")
		return
	}

	n := &store.NotificationRule{Event: req.Event, RoleID: req.RoleID, Address: req.Address, ChannelID: req.ChannelID}
	if err := s.store.CreateNotificationRule(r.Context(), n); err != nil {
		if store.IsDuplicate(err) {
			httpx.Fail(w, r, http.StatusConflict, "duplicate_rule", "That rule already exists.")
			return
		}
		httpx.Error(w, r, err)
		return
	}

	s.auditNotify(r, "notify.rule.create", req.Event)
	httpx.JSON(w, http.StatusCreated, n)
}

func (s *Server) handleDeleteNotifyRule(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteNotificationRule(r.Context(), r.PathValue("id")); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.auditNotify(r, "notify.rule.delete", r.PathValue("id"))
	httpx.JSON(w, http.StatusOK, okStatus)
}

// failedNotificationView is one dead-lettered message: what was being said, to whom, and
// the transport's own words for why it never arrived.
type failedNotificationView struct {
	Event   string    `json:"event"`
	Kind    string    `json:"kind"`
	To      string    `json:"to"`
	Subject string    `json:"subject"`
	Error   string    `json:"error"`
	At      time.Time `json:"at"`
}

// handleFailedNotifications is the dead-letter list. An alert that failed to send and then
// vanished is the worst outcome the system has: you would believe nothing went wrong.
func (s *Server) handleFailedNotifications(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.store.FailedMessages(r.Context(), 20)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]failedNotificationView, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, failedNotificationView{m.Event, m.Kind, m.To, m.Subject, m.LastError, m.CreatedAt})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (s *Server) auditNotify(r *http.Request, action, target string) {
	u, _ := auth.UserFrom(r.Context())
	e := store.AuditEntry{Action: action, Target: target, Outcome: "ok"}
	if u != nil {
		e.UserID, e.UserLabel = u.ID, u.Label()
	}
	s.audit(r.Context(), e)
}

// ── host watcher ────────────────────────────────────────────────────────────────

// watchHosts notices when a host stops answering, and says so once.
//
// Once is the whole design. A host that is down stays down, and an alert that repeats every
// minute until somebody fixes it is an alert that gets muted — after which the next one, for
// something else, is muted too. It fires on the TRANSITION, and again only if the host comes
// back and falls over a second time.
func (s *Server) watchHosts(ctx context.Context) {
	const interval = 60 * time.Second

	// Seeded on the first sweep rather than assumed online: a host that is already down when
	// Daffa starts should not produce an alert claiming it just went down.
	online := map[string]bool{}
	first := true

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		envs, err := s.store.ListEnvironments(ctx)
		if err != nil {
			slog.Error("host watcher: listing hosts", "err", err)
			continue
		}

		for _, e := range envs {
			// An environment is up if ANY of its nodes answers. A swarm with one node down is
			// degraded, not offline, and paging the team as though the whole cluster had gone is
			// how an alert channel gets muted. The node list is where a partial outage shows.
			up := false
			if env, err := s.pool.Get(e.ID); err == nil {
				pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				for _, n := range env.Nodes() {
					if n.Ping(pingCtx) == nil {
						up = true
						break
					}
				}
				cancel()
			}

			// Reconcile on the same sweep. Info().Swarm rides along on a daemon we have just
			// proved is reachable, so learning that a machine was promoted, demoted, or joined to
			// a Swarm costs nothing beyond a call already being made.
			if up {
				s.reconcileEnv(ctx, e.ID)
			}

			was, known := online[e.ID]
			online[e.ID] = up

			if first || !known || was == up {
				continue
			}
			if !up {
				s.notify.Send(ctx, e.ID, notify.Data{
					Event:    notify.AgentOffline,
					Subject:  "Host offline: " + e.Name,
					Title:    "Host offline: " + e.Name,
					Summary:  "The host “" + e.Name + "” stopped answering. Daffa cannot reach its Docker daemon.",
					HostName: e.Name,
					Target:   e.Name,
					Link:     "/host",
					Failed:   true,
				})
			}
		}
		first = false
	}
}
