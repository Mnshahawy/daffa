package store

import (
	"context"
	"errors"
	"testing"

	"github.com/Mnshahawy/daffa/internal/caps"
)

// The seeded roles are what every existing user is migrated onto, so their contents are
// load-bearing rather than cosmetic.
func TestSeededRoles(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		admin, err := s.AdminRole(ctx)
		if err != nil {
			t.Fatalf("AdminRole: %v", err)
		}
		if !admin.IsAdmin || !admin.Builtin {
			t.Fatalf("Admin role = %+v; want is_admin and builtin", admin)
		}
		// Resolved at runtime, not stored: a capability added tomorrow must be held by
		// admins the moment it exists.
		if !admin.Effective().Equal(caps.Everything) {
			t.Fatalf("the Admin role does not grant every capability")
		}

		op, err := s.RoleByName(ctx, "Operator")
		if err != nil {
			t.Fatalf("Operator role: %v", err)
		}
		eff := op.Effective()

		// Operator can run the fleet.
		for _, c := range []caps.Cap{caps.ContainersEdit, caps.StacksEdit, caps.BackupsEdit} {
			if !eff.Has(c) {
				t.Errorf("Operator lacks %s", c)
			}
		}
		// And must NOT hold any of the four standalone capabilities. This is the whole
		// point of separating them: shipping an Operator preset that quietly grants a root
		// shell would make the distinction decorative.
		for _, c := range []caps.Cap{caps.ContainersExec, caps.SystemPrune, caps.BackupsRestore, caps.BackupsDownload} {
			if eff.Has(c) {
				t.Errorf("Operator holds %s — edit must never imply it", c)
			}
		}
		// Nor the administrative objects.
		for _, c := range []caps.Cap{caps.UsersEdit, caps.RolesEdit, caps.SettingsEdit} {
			if eff.Has(c) {
				t.Errorf("Operator holds %s", c)
			}
		}

		viewer, err := s.RoleByName(ctx, "Viewer")
		if err != nil {
			t.Fatalf("Viewer role: %v", err)
		}
		veff := viewer.Effective()
		if !veff.Has(caps.ContainersView) || !veff.Has(caps.StacksView) {
			t.Error("Viewer cannot see containers or stacks")
		}
		for _, d := range caps.All {
			if d.Mode == caps.ModeEdit && veff.Has(d.Cap) {
				t.Errorf("Viewer holds the edit capability %s", d.Name)
			}
		}
	})
}

// The seeded masks are opaque integers in the migration's SQL, one per area, and an integer in
// SQL is a number nobody can read. This decodes them back into capability NAMES and pins the
// exact list.
//
// It is the only thing standing between a typo in a seed literal and a preset role that hands
// out something nobody chose. `('role_viewer', 'docker', 169)` looks no different from
// `('role_viewer', 'docker', 171)`, and the second one gives every viewer in a fresh install the
// power to delete images.
func TestSeededRolesGrantWhatTheyClaim(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		for _, c := range []struct {
			role string
			want []string
		}{
			{"Operator", []string{
				"containers.view", "containers.edit",
				"images.view", "images.edit",
				"networks.view", "networks.edit",
				"volumes.view", "volumes.edit",
				"stacks.view", "stacks.edit",
				"registries.view", "gitcreds.view",
				"backups.view", "backups.edit", "storage.view",
				"monitors.view", "monitors.edit", "audit.view",
				"hosts.view",
			}},
			{"Viewer", []string{
				"containers.view", "images.view", "networks.view", "volumes.view",
				"stacks.view",
				"backups.view", "storage.view",
				"monitors.view", "audit.view",
				"hosts.view",
			}},
		} {
			role, err := s.RoleByName(ctx, c.role)
			if err != nil {
				t.Fatalf("seeded role %q: %v", c.role, err)
			}

			want, err := caps.SetFromNames(c.want)
			if err != nil {
				t.Fatal(err)
			}
			if got := role.Effective(); !got.Equal(want) {
				t.Errorf("the seeded %s role does not grant what it claims.\n"+
					"  seeded: %v\n"+
					"expected: %v\n\n"+
					"The seed in migration 0009 is a set of raw integers, one per area. If you "+
					"changed the registry's bits, the seeds no longer mean what they used to — "+
					"recompute them. If you did not, one of those literals is a typo, and every "+
					"fresh install would get a preset role nobody chose.",
					c.role, got.Names(), want.Names())
			}
		}
	})
}

func TestEffectiveMaskUnionsRoles(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		u := &User{Kind: "local", Username: "sam"}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}

		// No roles ⇒ no capabilities. Fail closed.
		m, err := s.EffectiveMask(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !m.Global.IsZero() || len(m.Env) != 0 {
			t.Fatalf("a user with no roles has capabilities: %v", m.Global.Names())
		}

		backups := &Role{Name: "Backups", Caps: caps.Of(caps.BackupsEdit)}
		deploys := &Role{Name: "Deployers", Caps: caps.Of(caps.StacksEdit)}
		for _, r := range []*Role{backups, deploys} {
			if err := s.CreateRole(ctx, r); err != nil {
				t.Fatal(err)
			}
			if err := s.GrantRole(ctx, u.ID, r.ID, SourceLocal, Global()); err != nil {
				t.Fatal(err)
			}
		}

		sm, err := s.EffectiveMask(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		m = sm
		// The UNION of both roles — not the "highest" one. There is no highest one.
		for _, c := range []caps.Cap{caps.BackupsEdit, caps.StacksEdit} {
			if !m.Global.Has(c) {
				t.Errorf("effective mask lacks %s; got %v", c, m.Global.Names())
			}
		}
		// And edit implied view on the way in.
		if !m.Global.Has(caps.BackupsView) || !m.Global.Has(caps.StacksView) {
			t.Errorf("edit did not imply view: %v", m.Global.Names())
		}
		if m.Global.Has(caps.ContainersExec) {
			t.Error("effective mask granted a capability no role carried")
		}
	})
}

func TestAdminRoleGrantsEverythingAtRuntime(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		u := &User{Kind: "local", Username: "root"}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		admin, err := s.AdminRole(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.GrantRole(ctx, u.ID, admin.ID, SourceLocal, Global()); err != nil {
			t.Fatal(err)
		}

		m, err := s.EffectiveMask(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !m.Global.Equal(caps.Everything) {
			t.Fatalf("an admin does not hold every capability; missing %d bits",
				len(caps.All)-len(m.Global.Names()))
		}
	})
}

// Locking yourself out of your own console is the failure mode that turns a permissions
// feature into an outage. Every path that could do it must refuse.
func TestLockoutGuards(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		admin, err := s.AdminRole(ctx)
		if err != nil {
			t.Fatal(err)
		}

		root := &User{Kind: "local", Username: "root"}
		if err := s.CreateUser(ctx, root); err != nil {
			t.Fatal(err)
		}
		if err := s.GrantRole(ctx, root.ID, admin.ID, SourceLocal, Global()); err != nil {
			t.Fatal(err)
		}

		// root is now the ONLY administrator. All four of these must be refused.
		if err := s.RevokeRole(ctx, root.ID, admin.ID, Global()); !errors.Is(err, ErrLastAdmin) {
			t.Errorf("revoking the last admin role = %v; want ErrLastAdmin", err)
		}
		if err := s.SetUserDisabledGuarded(ctx, root.ID, true); !errors.Is(err, ErrLastAdmin) {
			t.Errorf("disabling the last admin = %v; want ErrLastAdmin", err)
		}
		if err := s.DeleteUserGuarded(ctx, root.ID); !errors.Is(err, ErrLastAdmin) {
			t.Errorf("deleting the last admin = %v; want ErrLastAdmin", err)
		}
		if err := s.DeleteRole(ctx, admin.ID); !errors.Is(err, ErrBuiltinRole) {
			t.Errorf("deleting the builtin Admin role = %v; want ErrBuiltinRole", err)
		}

		// Give someone else the admin role, and the guards must get out of the way — they
		// exist to prevent lockout, not to make administration impossible.
		second := &User{Kind: "local", Username: "second"}
		if err := s.CreateUser(ctx, second); err != nil {
			t.Fatal(err)
		}
		if err := s.GrantRole(ctx, second.ID, admin.ID, SourceLocal, Global()); err != nil {
			t.Fatal(err)
		}

		if err := s.RevokeRole(ctx, root.ID, admin.ID, Global()); err != nil {
			t.Errorf("revoking one of two admins was refused: %v", err)
		}
		if err := s.DeleteUserGuarded(ctx, root.ID); err != nil {
			t.Errorf("deleting a non-last admin was refused: %v", err)
		}

		// And the guard must count DISABLED admins as absent: a disabled admin cannot log
		// in, so leaning on one is the same as having none.
		third := &User{Kind: "local", Username: "third"}
		if err := s.CreateUser(ctx, third); err != nil {
			t.Fatal(err)
		}
		if err := s.GrantRole(ctx, third.ID, admin.ID, SourceLocal, Global()); err != nil {
			t.Fatal(err)
		}
		if err := s.SetUserDisabledGuarded(ctx, third.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := s.SetUserDisabledGuarded(ctx, second.ID, true); !errors.Is(err, ErrLastAdmin) {
			t.Errorf("disabling the last ENABLED admin = %v; want ErrLastAdmin "+
				"(a disabled admin cannot log in, so it does not count)", err)
		}
	})
}

// The IdP owns the roles it grants; Daffa owns the ones granted here. A re-sync must not
// silently delete an administrator's hand-granted role.
func TestSyncOIDCRolesLeavesLocalGrantsAlone(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		u := &User{Kind: "oidc", Sub: "abc", Email: "a@example.com"}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}

		fromIdP := &Role{Name: "FromIdP", Caps: caps.Of(caps.StacksView)}
		byHand := &Role{Name: "ByHand", Caps: caps.Of(caps.BackupsEdit)}
		stale := &Role{Name: "Stale", Caps: caps.Of(caps.ImagesView)}
		for _, r := range []*Role{fromIdP, byHand, stale} {
			if err := s.CreateRole(ctx, r); err != nil {
				t.Fatal(err)
			}
		}

		// The provider granted Stale last time; an admin granted ByHand here.
		if err := s.SyncOIDCRoles(ctx, u.ID, []ScopedGrant{{RoleID: stale.ID, Scope: Global()}}); err != nil {
			t.Fatal(err)
		}
		if err := s.GrantRole(ctx, u.ID, byHand.ID, SourceLocal, Global()); err != nil {
			t.Fatal(err)
		}

		// This login, the claim maps to FromIdP instead.
		if err := s.SyncOIDCRoles(ctx, u.ID, []ScopedGrant{{RoleID: fromIdP.ID, Scope: Global()}}); err != nil {
			t.Fatal(err)
		}

		got := map[string]string{}
		ms, err := s.UserRoles(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			got[m.Name] = m.Source
		}

		if got["FromIdP"] != SourceOIDC {
			t.Errorf("the newly claimed role was not granted: %v", got)
		}
		if _, still := got["Stale"]; still {
			t.Errorf("a role the IdP no longer grants survived the re-sync: %v", got)
		}
		if got["ByHand"] != SourceLocal {
			t.Errorf("the locally granted role was wiped by the IdP re-sync: %v", got)
		}
	})
}

// A user in two providers can legitimately have the same subject. Before the identity key
// became (provider, sub), the second one would have been logged in as the first.
func TestSubIsUniquePerProvider(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		p1 := &OIDCProvider{Slug: "corp", Name: "Corp", Issuer: "https://a.example.com",
			ClientID: "x", RedirectURL: "https://d/cb", Scopes: "openid", Enabled: true}
		p2 := &OIDCProvider{Slug: "partner", Name: "Partner", Issuer: "https://b.example.com",
			ClientID: "y", RedirectURL: "https://d/cb", Scopes: "openid", Enabled: true}
		for _, p := range []*OIDCProvider{p1, p2} {
			if err := s.CreateOIDCProvider(ctx, p); err != nil {
				t.Fatal(err)
			}
		}

		a := &User{Kind: "oidc", Sub: "1000", OIDCProvider: p1.ID, Email: "a@corp"}
		b := &User{Kind: "oidc", Sub: "1000", OIDCProvider: p2.ID, Email: "b@partner"}
		if err := s.CreateUser(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateUser(ctx, b); err != nil {
			t.Fatalf("the same sub at a DIFFERENT provider was rejected: %v", err)
		}

		got, err := s.UserBySub(ctx, p2.ID, "1000")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != b.ID {
			t.Fatalf("UserBySub returned the other provider's user (%s); the two collided", got.Email)
		}

		// The same sub at the SAME provider is still one person.
		dup := &User{Kind: "oidc", Sub: "1000", OIDCProvider: p1.ID}
		if err := s.CreateUser(ctx, dup); err == nil {
			t.Fatal("a duplicate sub within one provider was accepted")
		}
	})
}

func TestRolesForClaimsUnions(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		p := &OIDCProvider{Slug: "corp", Name: "Corp", Issuer: "https://a.example.com",
			ClientID: "x", RedirectURL: "https://d/cb", Scopes: "openid", RolesClaim: "groups", Enabled: true}
		if err := s.CreateOIDCProvider(ctx, p); err != nil {
			t.Fatal(err)
		}

		ops := &Role{Name: "Ops", Caps: caps.Of(caps.ContainersEdit)}
		backups := &Role{Name: "Backups", Caps: caps.Of(caps.BackupsEdit)}
		for _, r := range []*Role{ops, backups} {
			if err := s.CreateRole(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		for _, m := range []*OIDCRoleMapping{
			{ProviderID: p.ID, ClaimValue: "sre", RoleID: ops.ID},
			{ProviderID: p.ID, ClaimValue: "dba", RoleID: backups.ID},
		} {
			if err := s.CreateOIDCMapping(ctx, m); err != nil {
				t.Fatal(err)
			}
		}

		// Someone in both groups gets BOTH roles. Under the old model this reduced to the
		// single most privileged one, which has no meaning once permissions are a set.
		got, err := s.RolesForClaims(ctx, p.ID, []string{"sre", "dba", "unmapped"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("RolesForClaims returned %d roles; want 2 (the union)", len(got))
		}

		none, err := s.RolesForClaims(ctx, p.ID, []string{"nobody"})
		if err != nil {
			t.Fatal(err)
		}
		if len(none) != 0 {
			t.Fatalf("unmapped claims yielded roles: %v", none)
		}
	})
}

// A capability above bit 30 must survive a round trip through the database.
//
// This is a regression test for a bug that hid for a year behind SQLite. SQLite's INTEGER is
// 64-bit, so a mask with a high bit stored and read back perfectly. POSTGRES's INTEGER is
// 32-bit and signed — so roles.caps, declared INTEGER, could not hold bit 31 at all. The
// registry's own ceiling (caps.MaxBit) says 52, and nothing checked that the column agreed.
//
// It surfaced the moment monitors.view landed on bit 31 and a real Postgres refused the seed
// with "integer out of range". Had the seeded roles not happened to carry it, the failure would
// have waited for the first administrator to tick the box in the role editor — in production,
// against a mask they had every right to grant.
func TestAHighCapabilityBitSurvivesTheDatabase(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		// Every capability the registry has, which by construction includes the highest bit
		// currently defined. Not a hand-written constant: this test must keep telling the truth
		// as the registry grows, rather than pinning a number that goes stale.
		r := &Role{Name: "Everything", Description: "every bit", Caps: caps.Everything}
		if err := s.CreateRole(ctx, r); err != nil {
			t.Fatalf("storing a role with every capability: %v.\n\n"+
				"If this is \"integer out of range\", role_caps.mask is a 32-bit INTEGER on "+
				"Postgres and an area has outgrown it. It needs to be BIGINT.", err)
		}

		got, err := s.RoleByID(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Caps.Equal(caps.Everything) {
			t.Fatalf("the capability set did not survive the round trip:\n stored %v\n     got %v",
				caps.Everything.Names(), got.Caps.Names())
		}

		// Every area must have made it, not just the first one. A write path that forgot to loop
		// over namespaces would store one row, read one row, and pass any test that only asked
		// "does this role have SOME capabilities".
		for _, c := range []caps.Cap{
			caps.ContainersExec, caps.StacksEdit, caps.BackupsRestore,
			caps.MonitorsEdit, caps.UsersEdit,
		} {
			if !got.Caps.Has(c) {
				t.Errorf("%s (area %s) did not survive the round trip — an area is being dropped "+
					"on the way in or out", c.Name(), c.NS)
			}
		}
	})
}
