package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The keyring worker is the certificate worker's skeleton on a different payload: rotate
// what is due, then keep every delivery's volume matching the desired state. Hourly for
// the same reason — against schedules measured in days it is fine-grained enough that
// "rotated" and "delivered" land within the same coffee, and coarse enough to cost
// nothing (the content hash makes a no-change sweep free of Docker calls).
const keyringSweepInterval = time.Hour

func (s *Server) keyringWorker(ctx context.Context) {
	// First sweep shortly after boot, not an hour after: an instance that was down for a
	// month may owe several rotations. It performs ONE — the point of scheduled rotation
	// is a fresh key, not a count of them.
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.keyringSweep(ctx)
		t.Reset(keyringSweepInterval)
	}
}

func (s *Server) keyringSweep(ctx context.Context) {
	s.rotateDueKeyrings(ctx)

	// After rotating, converge the volumes. Content hashing makes the no-change case
	// free of Docker calls.
	deliveries, err := s.store.AllKeyringDeliveries(ctx)
	if err != nil {
		return
	}
	for _, d := range deliveries {
		if err := s.reportKeyringDeliverySync(ctx, d); err != nil {
			slog.Warn("keyring delivery sync failed", "delivery", d.ID, "volume", d.Volume, "err", err)
		}
	}
}

func (s *Server) rotateDueKeyrings(ctx context.Context) {
	list, err := s.store.ListKeyrings(ctx)
	if err != nil {
		return
	}
	now := time.Now()

	for _, k := range list {
		if k.RotateDays <= 0 {
			continue // manual rotation only
		}
		versions, err := s.store.KeyringVersions(ctx, k.ID)
		if err != nil {
			continue
		}
		var active *store.KeyringVersion
		for _, v := range versions {
			if v.State == store.KeyringVersionActive {
				active = v
				break
			}
		}
		if active == nil || now.Sub(active.CreatedAt) < time.Duration(k.RotateDays)*24*time.Hour {
			if active != nil {
				s.alarms().fire(k.ID+"/rotatefail", "") // healthy again ⇒ re-arm
			}
			continue
		}

		nv, err := s.rotateKeyring(ctx, k)
		if err != nil {
			slog.Warn("scheduled keyring rotation failed", "keyring", k.Name, "err", err)
			// Once per failure streak, not once per hour of it.
			if s.alarms().fire(k.ID+"/rotatefail", "failing") {
				s.notifyKeyring(ctx, notify.KeyringRotateFailed, k, true,
					fmt.Sprintf("Daffa tried to rotate “%s” and could not. Consumers keep encrypting with the current version; rotation will be retried hourly.", k.Name),
					err.Error())
			}
			continue
		}
		s.alarms().fire(k.ID+"/rotatefail", "")
		s.audit(ctx, store.AuditEntry{
			Action: "keyring.rotate", Target: k.Name, Outcome: "ok",
			Detail: store.AuditDetail(map[string]any{"automatic": true, "version": nv.ID}),
		})
		s.notifyKeyring(ctx, notify.KeyringRotated, k, false,
			fmt.Sprintf("Rotated “%s” on its %d-day schedule. New data encrypts under the new version; every prior version stays readable.", k.Name, k.RotateDays), "")
	}
}

func (s *Server) notifyKeyring(ctx context.Context, event notify.Event, k *store.Keyring, failed bool, summary, detail string) {
	title := map[notify.Event]string{
		notify.KeyringRotated:      "Keyring rotated: " + k.Name,
		notify.KeyringRotateFailed: "Keyring rotation failed: " + k.Name,
	}[event]
	s.notify.Send(ctx, "", notify.Data{
		Event: event, Subject: title, Title: title,
		Summary: summary, Target: k.Name,
		Detail: notify.Tail(detail, 20, 4000),
		Link:   "/keyrings", Failed: failed,
	})
}
