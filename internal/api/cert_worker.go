package api

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Mnshahawy/daffa/internal/certs"
	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The certificate worker is the daily renewal cron from internal-setup
// (renew-internal-certs.sh + internal-ca-check.cron), folded into the process and run
// hourly: re-sign what is inside its renewal window, nag about what cannot be re-signed,
// warn when a CA needs rotating, and keep every delivery's volume matching the desired
// state. An hour is fine-grained enough that "renewed" and "delivered" happen within the
// same coffee, and coarse enough to cost nothing.
const certSweepInterval = time.Hour

// certAlarms remembers which escalation stage each object was last notified at, so an
// hourly sweep does not send twenty-four copies of the same warning a day. In memory on
// purpose: a restart re-sends at most one round of warnings, which for material measured
// in months of validity is noise worth not carrying a table for.
type certAlarms struct {
	mu   sync.Mutex
	seen map[string]string // object id + kind → stage last notified
}

// fire reports whether this stage is NEW for the object — and records it.
func (a *certAlarms) fire(key, stage string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = map[string]string{}
	}
	if a.seen[key] == stage {
		return false
	}
	a.seen[key] = stage
	return true
}

func (s *Server) certWorker(ctx context.Context) {
	// First sweep shortly after boot, not an hour after: an instance that was down for a
	// week may be carrying a cert that expired on Tuesday.
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.certSweep(ctx)
		t.Reset(certSweepInterval)
	}
}

func (s *Server) certSweep(ctx context.Context) {
	s.sweepCertificates(ctx)
	s.sweepCAs(ctx)

	// After renewing and rotating, converge the volumes. Content hashing makes the
	// no-change case free of Docker calls.
	deliveries, err := s.store.AllCertDeliveries(ctx)
	if err != nil {
		return
	}
	for _, d := range deliveries {
		if err := s.reportDeliverySync(ctx, d); err != nil {
			slog.Warn("cert delivery sync failed", "delivery", d.ID, "volume", d.Volume, "err", err)
		}
	}
}

func (s *Server) sweepCertificates(ctx context.Context) {
	list, err := s.store.ListCertificates(ctx, true, nil) // the worker sweeps everything
	if err != nil {
		return
	}
	now := time.Now()

	for _, c := range list {
		window := time.Duration(c.RenewBeforeDays) * 24 * time.Hour
		left := c.NotAfter.Sub(now)
		if left > window {
			s.alarms().fire(c.ID+"/expiry", "") // back above the window ⇒ re-arm
			continue
		}

		if c.Issued() {
			if err := s.renewCertificate(ctx, c); err != nil {
				slog.Warn("certificate renewal failed", "cert", c.Name, "err", err)
				s.notifyCertFailure(ctx, c, left, err)
			} else {
				s.alarms().fire(c.ID+"/renewfail", "")
				s.notifyCert(ctx, notify.CertRenewed, c, false,
					fmt.Sprintf("Renewed “%s”: now valid until %s.", c.Name, c.NotAfter.Format("2006-01-02")), "")
			}
			continue
		}

		// Uploaded: Daffa cannot renew it, only say so — louder as it gets closer, once
		// per stage. The stages mirror the cron's mail policy (warn, then URGENT at 7 days).
		stage := "expiring"
		if left <= 0 {
			stage = "expired"
		} else if left <= 7*24*time.Hour {
			stage = "urgent"
		}
		if s.alarms().fire(c.ID+"/expiry", stage) {
			summary := map[string]string{
				"expiring": fmt.Sprintf("The uploaded certificate “%s” expires %s. Daffa cannot renew it — upload a replacement.", c.Name, c.NotAfter.Format("2006-01-02")),
				"urgent":   fmt.Sprintf("URGENT: “%s” expires in %d days, and Daffa cannot renew it. Upload a replacement now.", c.Name, int(left.Hours()/24)),
				"expired":  fmt.Sprintf("The certificate “%s” has EXPIRED. Everything serving it is now failing TLS verification.", c.Name),
			}[stage]
			s.notifyCert(ctx, notify.CertExpiring, c, true, summary, "")
		}
	}
}

// renewCertificate is the sweep's re-sign: same key, same SANs, verified before anything
// is replaced — a failed renewal leaves the old, still-valid certificate exactly where it
// was, which is the property the whole design leans on.
func (s *Server) renewCertificate(ctx context.Context, c *store.Certificate) error {
	ca, caKey, err := s.signingCA(ctx, c.CAID)
	if err != nil {
		return s.recordRenewError(ctx, c, err)
	}
	leafKey, err := s.sealer.Open(c.KeyEnc)
	if err != nil {
		return s.recordRenewError(ctx, c, fmt.Errorf("could not decrypt the certificate's key (was the master key replaced?)"))
	}
	renewed, err := certs.Renew(ca.CertPEM, caKey, c.CertPEM, leafKey, c.ValidityDays, c.Usages)
	if err != nil {
		return s.recordRenewError(ctx, c, err)
	}
	if err := certs.Verify(renewed, ca.CertPEM); err != nil {
		return s.recordRenewError(ctx, c, err)
	}

	parsed, _ := certs.ParseCert(renewed)
	c.CertPEM = renewed
	c.NotBefore, c.NotAfter = parsed.NotBefore, parsed.NotAfter
	c.Status, c.LastError = "ok", ""
	if err := s.store.UpdateCertificate(ctx, c); err != nil {
		return err
	}
	s.audit(ctx, store.AuditEntry{
		Action: "cert.renew", Target: c.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"automatic": true, "not_after": c.NotAfter}),
	})
	return nil
}

func (s *Server) recordRenewError(ctx context.Context, c *store.Certificate, err error) error {
	c.Status, c.LastError = "error", err.Error()
	_ = s.store.UpdateCertificate(ctx, c)
	return err
}

func (s *Server) notifyCertFailure(ctx context.Context, c *store.Certificate, left time.Duration, err error) {
	stage := "failing"
	if left <= 7*24*time.Hour {
		stage = "urgent"
	}
	if !s.alarms().fire(c.ID+"/renewfail", stage) {
		return
	}
	summary := fmt.Sprintf("Daffa tried to renew “%s” and could not. The current certificate is still valid until %s; renewal will be retried hourly.",
		c.Name, c.NotAfter.Format("2006-01-02"))
	if stage == "urgent" {
		summary = fmt.Sprintf("URGENT: “%s” expires in %d days and its renewal keeps FAILING.",
			c.Name, int(left.Hours()/24))
	}
	s.notifyCert(ctx, notify.CertRenewFailed, c, true, summary, err.Error())
}

func (s *Server) notifyCert(ctx context.Context, event notify.Event, c *store.Certificate, failed bool, summary, detail string) {
	title := map[notify.Event]string{
		notify.CertRenewed:     "Certificate renewed: " + c.Name,
		notify.CertRenewFailed: "Certificate renewal failed: " + c.Name,
		notify.CertExpiring:    "Certificate expiring: " + c.Name,
	}[event]
	s.notify.Send(ctx, "", notify.Data{
		Event: event, Subject: title, Title: title,
		Summary: summary, Target: c.Name,
		Detail: notify.Tail(detail, 20, 4000),
		Link:   "/certificates", Failed: failed,
	})
}

func (s *Server) sweepCAs(ctx context.Context) {
	cas, err := s.store.ListCertAuthorities(ctx)
	if err != nil {
		return
	}
	now := time.Now()

	for _, ca := range cas {
		switch ca.Status {
		case "active":
			// The weekly internal-ca-check cron: warn when the root is inside its warning
			// window, and stand down once a successor is staged.
			warn := time.Duration(ca.WarnDays) * 24 * time.Hour
			if ca.NotAfter.Sub(now) > warn {
				s.alarms().fire(ca.ID+"/rotate", "")
				continue
			}
			if s.hasStagedSuccessor(ctx, cas, ca.ID) {
				continue
			}
			if s.alarms().fire(ca.ID+"/rotate", "due") {
				s.notifyCA(ctx, ca,
					fmt.Sprintf("The CA “%s” expires %s.", ca.Name, ca.NotAfter.Format("2006-01-02")),
					"Stage a successor now (rotate), distribute the new root while both are trusted, then activate. The overlap is the safety margin — starting late is how it disappears.")
			}

		case "next":
			// A staged rotation that sailed past its own overlap window is a rotation
			// somebody forgot. Nag daily — never activate on a timer; activating with an
			// undistributed root is the one mistake this flow exists to prevent.
			if now.Before(ca.OverlapUntil) {
				continue
			}
			day := now.Format("2006-01-02")
			if s.alarms().fire(ca.ID+"/overdue", day) {
				s.notifyCA(ctx, ca,
					fmt.Sprintf("The staged CA “%s” passed its overlap deadline (%s) and is still not activated.", ca.Name, ca.OverlapUntil.Format("2006-01-02")),
					"If the new root is distributed everywhere, activate it. If not, that is the thing to finish — the old root keeps working meanwhile, but the rotation is stalled.")
			}
		}
	}
}

func (s *Server) hasStagedSuccessor(_ context.Context, cas []*store.CertAuthority, caID string) bool {
	for _, other := range cas {
		if other.Status == "next" && other.RotatesID == caID {
			return true
		}
	}
	return false
}

func (s *Server) notifyCA(ctx context.Context, ca *store.CertAuthority, summary, detail string) {
	title := "CA rotation: " + ca.Name
	s.notify.Send(ctx, "", notify.Data{
		Event: notify.CARotationDue, Subject: title, Title: title,
		Summary: summary, Target: ca.Name, Detail: detail,
		// CA management lives on the settings page; the cluster-scoped /certificates
		// page only shows the leaves.
		Link: "/settings/certificates", Failed: false,
	})
}

func (s *Server) alarms() *certAlarms { return s.certAlarms }
