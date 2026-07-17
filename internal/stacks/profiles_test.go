package stacks

import (
	"context"
	"testing"
)

// A service gated behind `profiles:` must appear in the parsed service list exactly when
// COMPOSE_PROFILES activates its profile — the same rule `docker compose` follows. This is
// what makes a profile-gated proxy (the deploy template's Traefik) show up in a stack's
// status view; without profile activation compose-go drops it to DisabledServices and the
// status join finds neither the service nor a "missing" row for it.
func TestParseHonoursComposeProfiles(t *testing.T) {
	const yaml = `
name: shop
services:
  web:
    image: nginx
  proxy:
    image: traefik:v3.6.7
    profiles: [edge]
`
	has := func(svcs []Service, name string) bool {
		for _, s := range svcs {
			if s.Name == name {
				return true
			}
		}
		return false
	}

	// No profile active: the gated service is absent, the ungated one is present.
	off, err := Parse(context.Background(), yaml, "shop", nil)
	if err != nil {
		t.Fatalf("Parse (no profile): %v", err)
	}
	if !has(off, "web") {
		t.Error("ungated service 'web' missing from parse")
	}
	if has(off, "proxy") {
		t.Error("profile-gated 'proxy' appeared without COMPOSE_PROFILES set")
	}

	// Profile active: the gated service now appears.
	on, err := Parse(context.Background(), yaml, "shop", []EnvVar{{Key: "COMPOSE_PROFILES", Value: "edge"}})
	if err != nil {
		t.Fatalf("Parse (profile on): %v", err)
	}
	if !has(on, "proxy") {
		t.Error("profile-gated 'proxy' missing even though COMPOSE_PROFILES=edge activates it")
	}
}
