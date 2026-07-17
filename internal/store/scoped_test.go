package store

import (
	"context"
	"errors"
	"testing"

	"github.com/Mnshahawy/daffa/internal/caps"
)

// twoHosts builds a prod and a staging environment to grant against.
func twoHosts(t *testing.T, s *Store) (prod, staging *Environment) {
	t.Helper()
	ctx := context.Background()

	prod = &Environment{Name: "prod"}
	staging = &Environment{Name: "staging"}
	for _, e := range []*Environment{prod, staging} {
		if err := s.CreateEnvironment(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	return prod, staging
}

// The load-bearing rule of the whole scoped model: a role carrying a global-only capability
// cannot be granted on one host.
//
// Without it, "Admin on staging" would be storable — and EffectiveMask short-circuits on
// is_admin, so it would resolve to administrator of the ENTIRE FLEET. The grant would say
// staging and the mask would say everything, and nothing would ever tell you.
func TestCannotScopeAGlobalOnlyRole(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		_, staging := twoHosts(t, s)

		u := &User{Kind: "local", Username: "sara"}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}

		admin, err := s.AdminRole(ctx)
		if err != nil {
			t.Fatal(err)
		}

		// The Admin role holds every capability, including users.edit — so it is global-only.
		err = s.GrantRole(ctx, u.ID, admin.ID, SourceLocal, OnEnv(staging.ID))
		if !errors.Is(err, ErrCannotScope) {
			t.Fatalf("granting Admin on one host = %v; want ErrCannotScope.\n"+
				"This is the rule the whole model rests on: a scoped admin grant would "+
				"resolve to admin of the whole fleet.", err)
		}

		// Any role carrying a global-only capability, not just Admin.
		userAdmin := &Role{Name: "User admin", Caps: caps.Of(caps.UsersEdit)}
		if err := s.CreateRole(ctx, userAdmin); err != nil {
			t.Fatal(err)
		}
		if err := s.GrantRole(ctx, u.ID, userAdmin.ID, SourceLocal, OnEnv(staging.ID)); !errors.Is(err, ErrCannotScope) {
			t.Errorf("granting a users.edit role on one host = %v; want ErrCannotScope", err)
		}

		// But globally, both are fine — the rule narrows WHERE a role may be granted, it
		// does not forbid the role.
		if err := s.GrantRole(ctx, u.ID, userAdmin.ID, SourceLocal, Global()); err != nil {
			t.Errorf("granting a users.edit role globally was refused: %v", err)
		}

		// And a role of purely env-scopable capabilities can be scoped.
		ops := &Role{Name: "Ops", Caps: caps.Of(caps.ContainersEdit)}
		if err := s.CreateRole(ctx, ops); err != nil {
			t.Fatal(err)
		}
		if err := s.GrantRole(ctx, u.ID, ops.ID, SourceLocal, OnEnv(staging.ID)); err != nil {
			t.Errorf("granting an ops role on one host was refused: %v", err)
		}
	})
}

// The point of the feature, end to end: capabilities on one host, nothing on the other.
func TestScopedGrantAppliesToOneHostOnly(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		prod, staging := twoHosts(t, s)

		u := &User{Kind: "local", Username: "sara"}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}

		op, err := s.RoleByName(ctx, "Operator")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.GrantRole(ctx, u.ID, op.ID, SourceLocal, OnEnv(staging.ID)); err != nil {
			t.Fatal(err)
		}

		m, err := s.EffectiveMask(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}

		if !m.Has(caps.StacksEdit, staging.ID) {
			t.Error("cannot deploy on the host they were granted")
		}
		if m.Has(caps.StacksEdit, prod.ID) {
			t.Error("a grant on staging leaked to prod — the whole feature is broken")
		}
		// And fleet-wide questions are answered NO. Being an operator somewhere is not
		// being an operator everywhere.
		if m.Has(caps.StacksEdit, "") {
			t.Error("a scoped grant satisfied a fleet-wide check")
		}
		if !m.Global.IsZero() {
			t.Errorf("a scoped grant leaked into the global mask: %v", m.Global.Names())
		}

		// Envs() is what the environment list filters on.
		if got := m.Envs(); len(got) != 1 || got[0] != staging.ID {
			t.Errorf("Envs() = %v; want just staging", got)
		}

		// HasAnywhere is the widening for the fleet-wide read lists, and must be true here.
		if !m.HasAnywhere(caps.GitCredsView) {
			t.Error("an operator on staging cannot see the git credential list, so they " +
				"cannot pick one when creating a stack")
		}
	})
}

// The same role, held on two hosts, plus a different one globally. All three coexist.
func TestSameRoleAtSeveralScopes(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		prod, staging := twoHosts(t, s)

		u := &User{Kind: "local", Username: "sara"}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}

		op, _ := s.RoleByName(ctx, "Operator")
		viewer, _ := s.RoleByName(ctx, "Viewer")

		for _, g := range []struct {
			role  string
			scope Scope
		}{
			{op.ID, OnEnv(staging.ID)},
			{op.ID, OnEnv(prod.ID)},
			{viewer.ID, Global()},
		} {
			if err := s.GrantRole(ctx, u.ID, g.role, SourceLocal, g.scope); err != nil {
				t.Fatalf("granting %v: %v", g.scope, err)
			}
		}

		ms, err := s.UserRoles(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(ms) != 3 {
			t.Fatalf("got %d memberships; want 3 — the same role at two scopes must be two grants", len(ms))
		}

		m, err := s.EffectiveMask(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Operator on both hosts…
		if !m.Has(caps.StacksEdit, staging.ID) || !m.Has(caps.StacksEdit, prod.ID) {
			t.Error("the two scoped Operator grants did not both apply")
		}
		// …and Viewer everywhere, which also means on both hosts.
		if !m.Has(caps.ContainersView, "") {
			t.Error("the global Viewer grant did not apply fleet-wide")
		}

		// Revoking one scope must leave the other standing. That is why the scope is part
		// of the grant's identity and not an attribute of it.
		if err := s.RevokeRole(ctx, u.ID, op.ID, OnEnv(staging.ID)); err != nil {
			t.Fatal(err)
		}
		m, err = s.EffectiveMask(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if m.Has(caps.StacksEdit, staging.ID) {
			t.Error("revoking Operator on staging did not take effect")
		}
		if !m.Has(caps.StacksEdit, prod.ID) {
			t.Error("revoking Operator on staging also revoked it on prod")
		}
	})
}

// A grant on a host that does not exist is a grant that silently does nothing. Refuse it.
func TestCannotGrantOnAnUnknownHost(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		u := &User{Kind: "local", Username: "sara"}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		op, _ := s.RoleByName(ctx, "Operator")

		if err := s.GrantRole(ctx, u.ID, op.ID, SourceLocal, OnEnv("no-such-host")); err == nil {
			t.Fatal("a grant on a nonexistent host was accepted")
		}
		// And the degenerate shapes: an env scope with no host, a global scope with one.
		if err := s.GrantRole(ctx, u.ID, op.ID, SourceLocal, Scope{Kind: ScopeEnv}); err == nil {
			t.Error("an env-scoped grant with no host was accepted — it would apply to nothing")
		}
		if err := s.GrantRole(ctx, u.ID, op.ID, SourceLocal, Scope{Kind: "nonsense", ID: "x"}); err == nil {
			t.Error("a grant with an unknown scope kind was accepted")
		}
	})
}

// When a host goes away, the grants scoped to it must go too — otherwise they dangle, and
// an agent re-enrolled onto the same id would silently restore somebody's access.
func TestRemovingAHostRevokesItsGrants(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		_, staging := twoHosts(t, s)

		u := &User{Kind: "local", Username: "sara"}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		op, _ := s.RoleByName(ctx, "Operator")
		if err := s.GrantRole(ctx, u.ID, op.ID, SourceLocal, OnEnv(staging.ID)); err != nil {
			t.Fatal(err)
		}

		if err := s.RevokeEnvGrants(ctx, staging.ID); err != nil {
			t.Fatal(err)
		}

		m, err := s.EffectiveMask(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if m.Has(caps.StacksEdit, staging.ID) || len(m.Envs()) != 0 {
			t.Error("a grant survived the removal of the host it was scoped to")
		}
	})
}

// The audit filter runs in SQL, and it has to: LIMIT is applied before rows come back, so a
// Go-side filter would silently return a short page.
func TestAuditIsFilteredInSQL(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		prod, staging := twoHosts(t, s)

		// Ten prod entries, then two on staging, then a fleet-level one with no host.
		for i := 0; i < 10; i++ {
			if err := s.Audit(ctx, AuditEntry{EnvID: prod.ID, Action: "container.stop", Outcome: "ok"}); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 2; i++ {
			if err := s.Audit(ctx, AuditEntry{EnvID: staging.ID, Action: "container.stop", Outcome: "ok"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Audit(ctx, AuditEntry{Action: "user.create", Outcome: "ok"}); err != nil {
			t.Fatal(err)
		}

		// A staging-scoped reader asking for 5 must get staging's TWO — not "5 rows, of
		// which 2 survived a filter", and certainly not prod's.
		got, err := s.ListAudit(ctx, 5, false, []string{staging.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("a staging-scoped reader saw %d entries; want 2. If this is 0 or 1, the "+
				"filter is running AFTER the LIMIT and the page is being silently truncated.", len(got))
		}
		for _, e := range got {
			if e.EnvID != staging.ID {
				t.Errorf("a staging-scoped reader saw an entry from %s", e.EnvID)
			}
		}

		// The fleet-level entry (no host) is for global readers only: who was granted what
		// is not a staging operator's business.
		for _, e := range got {
			if e.Action == "user.create" {
				t.Error("a scoped reader saw a fleet-level audit entry")
			}
		}

		// A global reader sees everything, including the host-less entry.
		all, err := s.ListAudit(ctx, 50, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 13 {
			t.Errorf("a global reader saw %d entries; want 13", len(all))
		}

		// Holding audit.view nowhere yields nothing — NOT everything, which is what an
		// empty `WHERE env_id IN ()` would have produced.
		none, err := s.ListAudit(ctx, 50, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(none) != 0 {
			t.Fatalf("a reader with no audit access saw %d entries — the empty filter fell "+
				"through to an unfiltered query", len(none))
		}
	})
}

// Same for the stack and backup lists.
func TestListsAreFilteredByHost(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		prod, staging := twoHosts(t, s)

		for _, e := range []*Environment{prod, staging} {
			if err := s.CreateStack(ctx, &Stack{
				EnvID: e.ID, Name: "app-" + e.Name, SourceKind: "inline", InlineYAML: "x",
			}); err != nil {
				t.Fatal(err)
			}
		}

		got, err := s.ListStacks(ctx, false, []string{staging.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].EnvID != staging.ID {
			t.Fatalf("a staging-scoped user saw %d stacks; want only staging's", len(got))
		}

		// No hosts at all ⇒ no stacks. The empty-IN-list trap again.
		none, err := s.ListStacks(ctx, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(none) != 0 {
			t.Fatalf("a user with no hosts saw %d stacks; want 0", len(none))
		}

		all, err := s.ListStacks(ctx, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 2 {
			t.Errorf("a global user saw %d stacks; want 2", len(all))
		}
	})
}
