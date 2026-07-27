package store

import (
	"context"
	"errors"
	"testing"
)

// fleetWorld builds two environments, a CA per environment's trust domain, and an
// env-scoped "cell-manager" leaf in each — the Wali shape fleet deliveries exist for.
func fleetWorld(t *testing.T, s *Store) (prod, staging *Environment, prodCA, stagingCA *CertAuthority, prodLeaf, stagingLeaf *Certificate) {
	t.Helper()
	ctx := context.Background()
	prod, staging = twoHosts(t, s)

	prodCA = &CertAuthority{Name: "prod-ca", Subject: "CN=prod-ca", CertPEM: "PEM-PROD-CA", KeyEnc: "sealed"}
	stagingCA = &CertAuthority{Name: "staging-ca", Subject: "CN=staging-ca", CertPEM: "PEM-STAGING-CA", KeyEnc: "sealed"}
	for _, ca := range []*CertAuthority{prodCA, stagingCA} {
		if err := s.CreateCertAuthority(ctx, ca); err != nil {
			t.Fatal(err)
		}
	}

	prodLeaf = &Certificate{Name: "cell-manager", EnvID: prod.ID, CAID: prodCA.ID,
		SANs: "cell-manager", CertPEM: "PEM-PROD-LEAF", KeyEnc: "sealed", Usages: "client"}
	stagingLeaf = &Certificate{Name: "cell-manager", EnvID: staging.ID, CAID: stagingCA.ID,
		SANs: "cell-manager", CertPEM: "PEM-STAGING-LEAF", KeyEnc: "sealed", Usages: "client"}
	for _, c := range []*Certificate{prodLeaf, stagingLeaf} {
		if err := s.CreateCertificate(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestFleetDeliveryRoundTrip(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		prod, staging, prodCA, stagingCA, prodLeaf, stagingLeaf := fleetWorld(t, s)

		d := &FleetDelivery{
			EnvID: prod.ID, Volume: "wali-fleet-certs", UID: 100, GID: 100,
			Groups: []FleetGroup{
				{Subdir: "eu-west-prod", BundleCAs: prodCA.ID, CertIDs: []string{prodLeaf.ID}},
				{Subdir: "eu-west-staging", CertIDs: []string{stagingLeaf.ID}}, // derived trust
			},
		}
		if err := s.CreateFleetDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}

		got, err := s.FleetDeliveryByID(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.EnvID != prod.ID || got.Volume != "wali-fleet-certs" || got.UID != 100 {
			t.Errorf("delivery mangled on round-trip: %+v", got)
		}
		if len(got.Groups) != 2 {
			t.Fatalf("got %d groups; want 2", len(got.Groups))
		}
		// Groups come back ordered by subdir; that order feeds the content hash.
		if got.Groups[0].Subdir != "eu-west-prod" || got.Groups[1].Subdir != "eu-west-staging" {
			t.Errorf("groups out of order: %q, %q", got.Groups[0].Subdir, got.Groups[1].Subdir)
		}
		if got.Groups[0].BundleCAs != prodCA.ID || got.Groups[1].BundleCAs != "" {
			t.Errorf("bundle selections mangled: %q, %q", got.Groups[0].BundleCAs, got.Groups[1].BundleCAs)
		}
		if len(got.Groups[0].CertIDs) != 1 || got.Groups[0].CertIDs[0] != prodLeaf.ID {
			t.Errorf("prod group certs = %v; want [%s]", got.Groups[0].CertIDs, prodLeaf.ID)
		}

		// An update replaces the groups wholesale — dropping a group must drop its certs.
		got.Groups = got.Groups[:1]
		got.RestartTargets = "wali"
		if err := s.UpdateFleetDelivery(ctx, got); err != nil {
			t.Fatal(err)
		}
		got, err = s.FleetDeliveryByID(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Groups) != 1 || got.RestartTargets != "wali" {
			t.Errorf("update did not stick: %+v", got)
		}

		// Visibility is keyed on the CONSUMER environment — where the volume lives — not
		// on the environments whose material the delivery carries.
		visible, err := s.ListFleetDeliveries(ctx, false, []string{prod.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(visible) != 1 {
			t.Errorf("a prod-scoped viewer saw %d fleet deliveries; want 1", len(visible))
		}
		invisible, err := s.ListFleetDeliveries(ctx, false, []string{staging.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(invisible) != 0 {
			t.Errorf("a staging-scoped viewer saw %d fleet deliveries; want 0 — the volume "+
				"lives on prod, and carrying staging's cert does not move the delivery there", len(invisible))
		}
		none, err := s.ListFleetDeliveries(ctx, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(none) != 0 {
			t.Error("a viewer with no hosts saw fleet deliveries — the empty-IN-list trap")
		}

		if err := s.MarkFleetDeliverySynced(ctx, d.ID, "hash1", nil); err != nil {
			t.Fatal(err)
		}
		got, _ = s.FleetDeliveryByID(ctx, d.ID)
		if got.Status != "ok" || got.SyncedHash != "hash1" || got.SyncedAt.IsZero() {
			t.Errorf("sync outcome not recorded: %+v", got)
		}

		if err := s.DeleteFleetDelivery(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FleetDeliveryByID(ctx, d.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("deleted delivery still readable: %v", err)
		}
		_ = stagingCA
	})
}

// The refuse-don't-orphan hooks: a certificate or CA a fleet delivery depends on counts
// as in-use, exactly as it would for a certificate delivery.
func TestFleetDeliveryCountsAsInUse(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		prod, _, prodCA, stagingCA, prodLeaf, stagingLeaf := fleetWorld(t, s)

		d := &FleetDelivery{
			EnvID: prod.ID, Volume: "wali-fleet-certs",
			Groups: []FleetGroup{
				{Subdir: "eu-west-prod", BundleCAs: prodCA.ID, CertIDs: []string{prodLeaf.ID}},
				{Subdir: "eu-west-staging", CertIDs: []string{stagingLeaf.ID}},
			},
		}
		if err := s.CreateFleetDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}

		if n, err := s.CertificateInUse(ctx, prodLeaf.ID); err != nil || n != 1 {
			t.Errorf("CertificateInUse(carried by a fleet delivery) = %d, %v; want 1 — "+
				"deleting it would strand the consumer", n, err)
		}
		if n, err := s.CertAuthorityInUse(ctx, prodCA.ID); err != nil || n < 2 {
			// One leaf + one fleet selection.
			t.Errorf("CertAuthorityInUse(selected by a fleet group) = %d, %v; want >= 2", n, err)
		}
		// stagingCA is selected by NO group (the staging group derives) — only its leaf
		// counts, so retiring the selection story stays accurate.
		if n, err := s.CertAuthorityInUse(ctx, stagingCA.ID); err != nil || n != 1 {
			t.Errorf("CertAuthorityInUse(derived-only) = %d, %v; want 1 (just the leaf)", n, err)
		}
	})
}

// Rotation activation rewrites explicit fleet selections along the lineage, exactly as it
// does certificate-delivery bundles. Derived groups need nothing — they follow ca_id.
func TestReplaceCAInFleetBundles(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		prod, _, prodCA, _, prodLeaf, stagingLeaf := fleetWorld(t, s)

		successor := &CertAuthority{Name: "prod-ca-2", Subject: "CN=prod-ca-2",
			CertPEM: "PEM-PROD-CA-2", KeyEnc: "sealed", Status: "next", RotatesID: prodCA.ID}
		if err := s.CreateCertAuthority(ctx, successor); err != nil {
			t.Fatal(err)
		}

		d := &FleetDelivery{
			EnvID: prod.ID, Volume: "wali-fleet-certs",
			Groups: []FleetGroup{
				{Subdir: "eu-west-prod", BundleCAs: prodCA.ID + " " + "ca_unrelated", CertIDs: []string{prodLeaf.ID}},
				{Subdir: "eu-west-staging", CertIDs: []string{stagingLeaf.ID}},
			},
		}
		if err := s.CreateFleetDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}

		if err := s.ReplaceCAInFleetBundles(ctx, prodCA.ID, successor.ID); err != nil {
			t.Fatal(err)
		}
		got, err := s.FleetDeliveryByID(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if want := successor.ID + " ca_unrelated"; got.Groups[0].BundleCAs != want {
			t.Errorf("selection after activation = %q; want %q — a delivery still naming "+
				"the retired root would lose the new one when the overlap ends", got.Groups[0].BundleCAs, want)
		}
		if got.Groups[1].BundleCAs != "" {
			t.Errorf("a derived group grew a selection: %q", got.Groups[1].BundleCAs)
		}
	})
}
