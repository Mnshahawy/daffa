package api

import (
	"strings"
	"testing"

	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The fragment is what Traefik actually reads, so its shape is the contract: every carried
// certificate listed, the paths rooted at the mount path the OPERATOR declared (Traefik
// resolves them in its own filesystem, not Daffa's), and the stores block present only when
// a default was chosen.
func TestTraefikFragment(t *testing.T) {
	got := traefikFragment("/etc/traefik/dynamic", []string{"api", "web"}, "web")
	for _, want := range []string{
		"certFile: /etc/traefik/dynamic/api.crt",
		"keyFile: /etc/traefik/dynamic/api.key",
		"certFile: /etc/traefik/dynamic/web.crt",
		"defaultCertificate:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fragment missing %q:\n%s", want, got)
		}
	}
	// The default appears twice — once as the store default, once in the certificates list
	// — and every other certificate exactly once.
	if n := strings.Count(got, "/etc/traefik/dynamic/web.crt"); n != 2 {
		t.Errorf("default certificate appears %d times, want 2 (store default + list):\n%s", n, got)
	}

	// No default: the stores block is omitted rather than guessed at, and Traefik keeps its
	// own self-signed default for unmatched SNI.
	none := traefikFragment("/etc/traefik/dynamic", []string{"api"}, "")
	if strings.Contains(none, "stores:") || strings.Contains(none, "defaultCertificate") {
		t.Errorf("a delivery with no default certificate must not render a stores block:\n%s", none)
	}
	if !strings.Contains(none, "certFile: /etc/traefik/dynamic/api.crt") {
		t.Errorf("certificates list missing:\n%s", none)
	}
}

func TestCleanMountPath(t *testing.T) {
	if got, err := cleanMountPath(""); err != nil || got != store.DefaultCertMountPath {
		t.Errorf("empty mount path = %q, %v; want the default", got, err)
	}
	if got, err := cleanMountPath("/etc/traefik/dynamic/"); err != nil || got != "/etc/traefik/dynamic" {
		t.Errorf("trailing slash = %q, %v; want it cleaned", got, err)
	}
	for _, bad := range []string{"etc/traefik", "/", "/etc/../.."} {
		if _, err := cleanMountPath(bad); err == nil {
			t.Errorf("mount path %q was accepted; it must be refused", bad)
		}
	}
}

// Two certificates with the same NAME want the same filename in the volume, one silently
// overwriting the other. Names are unique per environment, so this needs a SHARED
// certificate and an env-scoped one — exactly the pair the uniqueness index allows.
func TestDeliveryRefusesCollidingCertNames(t *testing.T) {
	s, ctx := certServer(t)

	env := &store.Environment{Name: "prod"}
	if err := s.store.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	shared := &store.Certificate{Name: "web", CertPEM: "p", KeyEnc: "k"}
	scoped := &store.Certificate{Name: "web", EnvID: env.ID, CertPEM: "p", KeyEnc: "k"}
	for _, c := range []*store.Certificate{shared, scoped} {
		if err := s.store.CreateCertificate(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	_, code, msg := s.resolveDeliveryCerts(ctx, env.ID, []deliveryCertRequest{
		{CertID: shared.ID}, {CertID: scoped.ID},
	})
	if code != "name_collision" {
		t.Fatalf("two certificates named web were accepted (code %q, msg %q); both want web.crt", code, msg)
	}

	// One default at most: Traefik has a single stores.default.defaultCertificate.
	if _, code, _ := s.resolveDeliveryCerts(ctx, env.ID, []deliveryCertRequest{
		{CertID: shared.ID, IsDefault: true}, {CertID: shared.ID, IsDefault: true},
	}); code != "" {
		t.Fatalf("the same certificate listed twice should collapse, not fail: %q", code)
	}
	other := &store.Certificate{Name: "api", EnvID: env.ID, CertPEM: "p", KeyEnc: "k"}
	if err := s.store.CreateCertificate(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, code, _ := s.resolveDeliveryCerts(ctx, env.ID, []deliveryCertRequest{
		{CertID: scoped.ID, IsDefault: true}, {CertID: other.ID, IsDefault: true},
	}); code != "multiple_defaults" {
		t.Fatalf("two default certificates were accepted (code %q)", code)
	}
}

// One volume, one tls.yml, one owner. Two Traefik deliveries would rewrite each other
// forever while both reported ok, because a delivery's synced_hash covers only its own
// desired state — the failure the unique index and this refusal exist to end.
func TestSecondTraefikDeliveryOnAVolumeIsRefused(t *testing.T) {
	s, ctx := certServer(t)

	env := &store.Environment{Name: "prod"}
	if err := s.store.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	first := &store.CertDelivery{EnvID: env.ID, Volume: "traefik-dynamic", Traefik: true}
	if err := s.store.CreateCertDelivery(ctx, first); err != nil {
		t.Fatal(err)
	}

	second := &store.CertDelivery{EnvID: env.ID, Volume: "traefik-dynamic", Traefik: true}
	if err := s.refuseSecondTraefikDelivery(ctx, second); err == nil {
		t.Fatal("a second Traefik delivery into the same volume was allowed")
	}
	// The store refuses it too, so an API caller that bypasses the handler check cannot
	// create the pair either.
	if err := s.store.CreateCertDelivery(ctx, second); err == nil {
		t.Fatal("the unique index did not stop a second Traefik delivery on the volume")
	}

	// Same volume without the fragment is fine: disjoint PEMs, no tls.yml to fight over.
	plain := &store.CertDelivery{EnvID: env.ID, Volume: "traefik-dynamic"}
	if err := s.refuseSecondTraefikDelivery(ctx, plain); err != nil {
		t.Fatalf("a non-Traefik delivery should share the volume freely: %v", err)
	}
	if err := s.store.CreateCertDelivery(ctx, plain); err != nil {
		t.Fatalf("a non-Traefik delivery should share the volume freely: %v", err)
	}
	// And editing the owner in place is not a collision with itself.
	if err := s.refuseSecondTraefikDelivery(ctx, first); err != nil {
		t.Fatalf("a delivery collided with itself: %v", err)
	}
}

// The mixed dynamic directory works because the two writers own disjoint filenames. A repo
// that carries its own tls.yml breaks that, so the sync refuses instead of flapping.
func TestVolumeSourceRefusesDeliveryOwnedFilenames(t *testing.T) {
	s, ctx := certServer(t)

	env := &store.Environment{Name: "prod"}
	if err := s.store.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	cert := &store.Certificate{Name: "web", EnvID: env.ID, CertPEM: "p", KeyEnc: "k"}
	if err := s.store.CreateCertificate(ctx, cert); err != nil {
		t.Fatal(err)
	}
	d := &store.CertDelivery{EnvID: env.ID, Volume: "traefik-dynamic", Traefik: true,
		Certs: []store.DeliveryCert{{CertID: cert.ID, IsDefault: true}}}
	if err := s.store.CreateCertDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}

	src := &store.VolumeSource{EnvID: env.ID, Volume: "traefik-dynamic"}
	// Both entry points must agree, because they are the same rule asked at two moments:
	// the inline save (names straight off the request) and the sync (names off a resolved
	// subtree). A file refused by one and accepted by the other is how a volume ends up
	// with two writers fighting over one filename.
	for _, name := range []string{"tls.yml", "web.crt", "web.key", "ca-bundle.crt", ".daffa-certs-manifest"} {
		tree := &stacks.ResolvedTree{Files: []stacks.TreeFile{{Name: name, Data: []byte("x")}}}
		syncErr := s.refuseDeliveryFileClash(ctx, src, tree)
		saveErr := s.refuseDeliveryOwnedNames(ctx, env.ID, "traefik-dynamic", []string{name})
		if syncErr == nil || !strings.Contains(syncErr.Error(), name) {
			t.Errorf("sync accepted a source carrying %s (err %v); it must be refused by name", name, syncErr)
		}
		if saveErr == nil || !strings.Contains(saveErr.Error(), name) {
			t.Errorf("save accepted a source carrying %s (err %v); the pre-flight must refuse it too", name, saveErr)
		}
	}

	// The whole point: middlewares beside the certificates are fine, on both paths.
	fragments := []string{"middlewares.yml", "routers.yml"}
	ok := &stacks.ResolvedTree{Files: []stacks.TreeFile{
		{Name: fragments[0], Data: []byte("x")}, {Name: fragments[1], Data: []byte("x")},
	}}
	if err := s.refuseDeliveryFileClash(ctx, src, ok); err != nil {
		t.Fatalf("config fragments must be allowed to share the volume: %v", err)
	}
	if err := s.refuseDeliveryOwnedNames(ctx, env.ID, "traefik-dynamic", fragments); err != nil {
		t.Fatalf("the pre-flight must allow config fragments: %v", err)
	}

	// A source on a DIFFERENT volume is unaffected, tls.yml or not.
	elsewhere := &store.VolumeSource{EnvID: env.ID, Volume: "other-dynamic"}
	clash := &stacks.ResolvedTree{Files: []stacks.TreeFile{{Name: "tls.yml", Data: []byte("x")}}}
	if err := s.refuseDeliveryFileClash(ctx, elsewhere, clash); err != nil {
		t.Fatalf("a source on another volume must not be refused: %v", err)
	}
	if err := s.refuseDeliveryOwnedNames(ctx, env.ID, "other-dynamic", []string{"tls.yml"}); err != nil {
		t.Fatalf("the pre-flight must not reach across volumes: %v", err)
	}

	// A NON-Traefik delivery renders no fragment, so it owns nothing to clash with.
	plainEnv := &store.Environment{Name: "staging"}
	if err := s.store.CreateEnvironment(ctx, plainEnv); err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateCertDelivery(ctx, &store.CertDelivery{
		EnvID: plainEnv.ID, Volume: "plain-vol",
		Certs: []store.DeliveryCert{{CertID: cert.ID}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.refuseDeliveryOwnedNames(ctx, plainEnv.ID, "plain-vol", []string{"tls.yml"}); err != nil {
		t.Fatalf("a delivery that renders no fragment owns no filenames: %v", err)
	}
}
