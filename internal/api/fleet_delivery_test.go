package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Mnshahawy/daffa/internal/certs"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The validation surface: everything resolveFleetGroups refuses, and the one thing it
// must NOT refuse — a certificate from another environment, which is the entire feature.
func TestFleetGroupsValidation(t *testing.T) {
	s, ctx := certServer(t)

	prod := &store.Environment{Name: "prod"}
	staging := &store.Environment{Name: "staging"}
	for _, e := range []*store.Environment{prod, staging} {
		if err := s.store.CreateEnvironment(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	prodLeaf := &store.Certificate{Name: "cell-manager", EnvID: prod.ID, CertPEM: "p", KeyEnc: "k"}
	stagingLeaf := &store.Certificate{Name: "cell-manager", EnvID: staging.ID, CertPEM: "p", KeyEnc: "k"}
	for _, c := range []*store.Certificate{prodLeaf, stagingLeaf} {
		if err := s.store.CreateCertificate(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	// The point of the feature: both environments' leaves, one request, accepted —
	// because each source sits in its own subdirectory.
	groups, code, msg := s.resolveFleetGroups(ctx, []fleetGroupRequest{
		{Subdir: "eu-west-prod", Certs: []string{prodLeaf.ID}},
		{Subdir: "eu-west-staging", Certs: []string{stagingLeaf.ID}},
	})
	if code != "" {
		t.Fatalf("the cross-environment case was refused (code %q, msg %q) — it is what fleet deliveries are FOR", code, msg)
	}
	if len(groups) != 2 {
		t.Fatalf("got %d groups; want 2", len(groups))
	}

	// Same-named certificates in ONE group fight over one filename; the subdir is the fix.
	if _, code, _ := s.resolveFleetGroups(ctx, []fleetGroupRequest{
		{Subdir: "all", Certs: []string{prodLeaf.ID, stagingLeaf.ID}},
	}); code != "name_collision" {
		t.Errorf("two cell-managers in one subdir accepted (code %q); both want cell-manager.crt there", code)
	}

	refusals := []struct {
		name string
		req  []fleetGroupRequest
		code string
	}{
		{"no groups", nil, "no_groups"},
		{"bad subdir", []fleetGroupRequest{{Subdir: "Has/Dots."}}, "bad_subdir"},
		{"uppercase subdir", []fleetGroupRequest{{Subdir: "EU-West"}}, "bad_subdir"},
		{"duplicate subdir", []fleetGroupRequest{{Subdir: "a"}, {Subdir: "a"}}, "duplicate_subdir"},
		{"cert in two groups", []fleetGroupRequest{
			{Subdir: "a", Certs: []string{prodLeaf.ID}},
			{Subdir: "b", Certs: []string{prodLeaf.ID}},
		}, "cert_in_two_groups"},
		{"unknown cert", []fleetGroupRequest{{Subdir: "a", Certs: []string{"crt_nope"}}}, "no_such_cert"},
		{"unknown CA", []fleetGroupRequest{{Subdir: "a", BundleCAs: []string{"ca_nope"}}}, "no_such_ca"},
	}
	for _, tc := range refusals {
		if _, code, _ := s.resolveFleetGroups(ctx, tc.req); code != tc.code {
			t.Errorf("%s: code = %q; want %q", tc.name, code, tc.code)
		}
	}

	// A staged successor is refused by the same rule as cert deliveries: select the
	// incumbent, the successor rides along by lineage.
	incumbent := &store.CertAuthority{Name: "prod-ca", Subject: "CN=prod-ca", CertPEM: "P", KeyEnc: "k"}
	if err := s.store.CreateCertAuthority(ctx, incumbent); err != nil {
		t.Fatal(err)
	}
	staged := &store.CertAuthority{Name: "prod-ca-2", Subject: "CN=prod-ca-2", CertPEM: "P2",
		KeyEnc: "k", Status: "next", RotatesID: incumbent.ID}
	if err := s.store.CreateCertAuthority(ctx, staged); err != nil {
		t.Fatal(err)
	}
	if _, code, _ := s.resolveFleetGroups(ctx, []fleetGroupRequest{
		{Subdir: "a", BundleCAs: []string{staged.ID}},
	}); code != "staged_ca" {
		t.Errorf("a staged successor in a group selection was accepted (code %q)", code)
	}

	// A repeat within one group collapses rather than fails — the resolveDeliveryCerts rule.
	groups, code, _ = s.resolveFleetGroups(ctx, []fleetGroupRequest{
		{Subdir: "a", Certs: []string{prodLeaf.ID, prodLeaf.ID, ""}},
	})
	if code != "" || len(groups[0].CertIDs) != 1 {
		t.Errorf("a repeated cert within a group should collapse: code %q, certs %v", code, groups[0].CertIDs)
	}
}

// The rendered file set — the contract a consumer like Wali reads: per-subdir cert, key
// and trust bundle, with the trust derived from the ISSUING CAs unless explicitly chosen,
// and no bundle at all for an uploaded-only group.
func TestFleetDeliveryFilesLayout(t *testing.T) {
	s, ctx := certServer(t)

	prod := &store.Environment{Name: "prod"}
	staging := &store.Environment{Name: "staging"}
	box := &store.Environment{Name: "box"}
	for _, e := range []*store.Environment{prod, staging, box} {
		if err := s.store.CreateEnvironment(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	// Real CAs, via the real handler: trustBundle parses PEM, so fakes will not do.
	prodCA := postJSON[caView](t, s.handleCreateCA, "POST", "/api/certs/cas", nil,
		`{"name":"prod-ca","common_name":"Prod CA"}`, http.StatusOK)
	stagingCA := postJSON[caView](t, s.handleCreateCA, "POST", "/api/certs/cas", nil,
		`{"name":"staging-ca","common_name":"Staging CA"}`, http.StatusOK)

	sealedKey, err := s.sealer.Seal("PEM-KEY\n")
	if err != nil {
		t.Fatal(err)
	}
	prodLeaf := &store.Certificate{Name: "cell-manager", EnvID: prod.ID, CAID: prodCA.ID,
		CertPEM: "PEM-PROD-LEAF\n", ChainPEM: "PEM-PROD-CHAIN\n", KeyEnc: sealedKey}
	stagingLeaf := &store.Certificate{Name: "cell-manager", EnvID: staging.ID, CAID: stagingCA.ID,
		CertPEM: "PEM-STAGING-LEAF\n", KeyEnc: sealedKey}
	uploaded := &store.Certificate{Name: "external", CertPEM: "PEM-EXT\n", KeyEnc: sealedKey} // no CAID
	rootCert := &store.Certificate{Name: "shared-web", CertPEM: "PEM-WEB\n", KeyEnc: sealedKey, CAID: prodCA.ID}
	for _, c := range []*store.Certificate{prodLeaf, stagingLeaf, uploaded, rootCert} {
		if err := s.store.CreateCertificate(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	d := &store.FleetDelivery{
		EnvID: box.ID, Volume: "wali-fleet-certs",
		Groups: []store.FleetGroup{
			{Subdir: "", CertIDs: []string{rootCert.ID}},                                         // the volume root is a group like any other
			{Subdir: "eu-west-prod", CertIDs: []string{prodLeaf.ID}},                             // derived trust
			{Subdir: "eu-west-staging", BundleCAs: prodCA.ID, CertIDs: []string{stagingLeaf.ID}}, // explicit trust, deliberately not the issuer
			{Subdir: "external", CertIDs: []string{uploaded.ID}},                                 // uploaded-only: no bundle
		},
	}
	if err := s.store.CreateFleetDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	d, err = s.store.FleetDeliveryByID(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}

	files, hash, err := s.fleetDeliveryFiles(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	roots := func(name string) []string {
		t.Helper()
		parsed, err := certs.ParseCerts(string(files[name]))
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		var cns []string
		for _, c := range parsed {
			cns = append(cns, c.Subject.CommonName)
		}
		return cns
	}

	// The chain rides in the .crt; the key unseals; subdirs prefix their files.
	if got := string(files["eu-west-prod/cell-manager.crt"]); got != "PEM-PROD-LEAF\nPEM-PROD-CHAIN\n" {
		t.Errorf("prod leaf .crt = %q; want leaf+chain", got)
	}
	if got := string(files["eu-west-prod/cell-manager.key"]); got != "PEM-KEY\n" {
		t.Errorf("prod leaf .key = %q; the sealed key must unseal into the volume", got)
	}
	// Derived trust: the issuing CA's lineage, nothing else — not staging's root, not "all".
	if got := roots("eu-west-prod/ca-bundle.crt"); len(got) != 1 || got[0] != "Prod CA" {
		t.Errorf("derived bundle = %v; want exactly the issuer's lineage", got)
	}
	// Explicit trust wins over derivation: the staging group carries a STAGING-issued
	// leaf but selects prod's root, and the bundle must say what was selected.
	if got := roots("eu-west-staging/ca-bundle.crt"); len(got) != 1 || got[0] != "Prod CA" {
		t.Errorf("explicit bundle = %v; the selection must override the issuer", got)
	}
	// Uploaded-only: no bundle file at all. Guessing trust looks like it worked.
	if _, ok := files["external/ca-bundle.crt"]; ok {
		t.Error("an uploaded-only group grew a ca-bundle.crt — Daffa cannot know an external issuer's anchors")
	}
	// The root group writes flat files beside the subdirs.
	if _, ok := files["shared-web.crt"]; !ok {
		t.Error("the root group's certificate did not land at the volume root")
	}
	if got := roots("ca-bundle.crt"); len(got) != 1 || got[0] != "Prod CA" {
		t.Errorf("root bundle = %v; want the root group's derived trust", got)
	}

	// The load-bearing cross-environment property: a renewal in a SOURCE environment
	// changes this delivery's desired hash, so the fleet-wide sweep rewrites the volume
	// in the CONSUMER's environment. Any future "optimize the sweep to the cert's own
	// env" breaks exactly this.
	prodLeaf.CertPEM = "PEM-PROD-LEAF-RENEWED\n"
	if err := s.store.UpdateCertificate(ctx, prodLeaf); err != nil {
		t.Fatal(err)
	}
	_, renewedHash, err := s.fleetDeliveryFiles(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if renewedHash == hash {
		t.Fatal("a source-env renewal did not change the delivery's desired hash — the volume would go stale")
	}

	// Rotation, phase one: a staged successor rides into the DERIVED bundle by lineage —
	// old and new roots together, which is what lets the source environment rotate
	// without a flag day for the fleet consumer.
	postJSON[caView](t, s.handleRotateCA, "POST", "/api/certs/cas/"+prodCA.ID+"/rotate",
		map[string]string{"id": prodCA.ID}, `{"overlap_days":30}`, http.StatusOK)
	files, rotHash, err := s.fleetDeliveryFiles(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if got := roots("eu-west-prod/ca-bundle.crt"); len(got) != 2 {
		t.Errorf("derived bundle during rotation = %v; want the incumbent AND the staged successor", got)
	}
	if rotHash == renewedHash {
		t.Error("staging a successor did not change the desired hash")
	}
}

// One volume, one delivery — across BOTH tables. A fleet delivery and a certificate
// delivery each mirror their own manifest, so sharing a volume would let them prune each
// other's files while both report ok.
func TestFleetVolumeExclusivity(t *testing.T) {
	s, ctx := certServer(t)

	env := &store.Environment{Name: "prod"}
	if err := s.store.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}

	certDlv := &store.CertDelivery{EnvID: env.ID, Volume: "shared-vol"}
	if err := s.store.CreateCertDelivery(ctx, certDlv); err != nil {
		t.Fatal(err)
	}
	fleet := &store.FleetDelivery{EnvID: env.ID, Volume: "shared-vol"}
	if err := s.refuseFleetVolumeClash(ctx, fleet); err == nil {
		t.Fatal("a fleet delivery was allowed onto a volume a certificate delivery writes")
	}

	fleet.Volume = "fleet-vol"
	if err := s.refuseFleetVolumeClash(ctx, fleet); err != nil {
		t.Fatalf("a free volume was refused: %v", err)
	}
	if err := s.store.CreateFleetDelivery(ctx, fleet); err != nil {
		t.Fatal(err)
	}
	// Editing the owner in place is not a collision with itself…
	if err := s.refuseFleetVolumeClash(ctx, fleet); err != nil {
		t.Fatalf("a fleet delivery collided with itself: %v", err)
	}
	// …but a second fleet delivery on the volume is: they would share one manifest.
	second := &store.FleetDelivery{EnvID: env.ID, Volume: "fleet-vol"}
	if err := s.refuseFleetVolumeClash(ctx, second); err == nil {
		t.Fatal("a second fleet delivery on the same volume was allowed")
	}
}

// A volume source sharing a fleet volume must not land a file where a fleet group writes
// — the mixed-directory rule, extended to the third writer's names.
func TestVolumeSourceRefusesFleetOwnedFilenames(t *testing.T) {
	s, ctx := certServer(t)

	box := &store.Environment{Name: "box"}
	prod := &store.Environment{Name: "prod"}
	for _, e := range []*store.Environment{box, prod} {
		if err := s.store.CreateEnvironment(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	leaf := &store.Certificate{Name: "cell-manager", EnvID: prod.ID, CertPEM: "p", KeyEnc: "k"}
	if err := s.store.CreateCertificate(ctx, leaf); err != nil {
		t.Fatal(err)
	}
	d := &store.FleetDelivery{EnvID: box.ID, Volume: "wali-fleet-certs",
		Groups: []store.FleetGroup{{Subdir: "eu-west-prod", CertIDs: []string{leaf.ID}}}}
	if err := s.store.CreateFleetDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}

	src := &store.VolumeSource{EnvID: box.ID, Volume: "wali-fleet-certs"}
	for _, name := range []string{
		"eu-west-prod/cell-manager.crt", "eu-west-prod/cell-manager.key",
		"eu-west-prod/ca-bundle.crt", ".daffa-fleet-manifest",
	} {
		tree := &stacks.ResolvedTree{Files: []stacks.TreeFile{{Name: name, Data: []byte("x")}}}
		syncErr := s.refuseDeliveryFileClash(ctx, src, tree)
		saveErr := s.refuseFleetOwnedNames(ctx, box.ID, "wali-fleet-certs", []string{name})
		if syncErr == nil || !strings.Contains(syncErr.Error(), name) {
			t.Errorf("sync accepted a source carrying %s (err %v)", name, syncErr)
		}
		if saveErr == nil || !strings.Contains(saveErr.Error(), name) {
			t.Errorf("save accepted a source carrying %s (err %v)", name, saveErr)
		}
	}
	// A name the fleet delivery does not own passes: sources share the volume freely.
	if err := s.refuseFleetOwnedNames(ctx, box.ID, "wali-fleet-certs", []string{"config.yml"}); err != nil {
		t.Errorf("an unowned filename was refused: %v", err)
	}
}
