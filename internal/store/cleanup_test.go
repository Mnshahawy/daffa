package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// The precedence the worker depends on: an override wins over the fleet default, INCLUDING
// a disabled one. "This host does not get swept" is the main reason to override a
// fleet-wide sweep, so an override that falls through to the global default when it is
// switched off would sweep exactly the host someone had exempted.
func TestEffectiveCleanupPolicyPrefersTheOverride(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		env := &Environment{Name: "prod"}
		exempt := &Environment{Name: "build-box"}
		for _, e := range []*Environment{env, exempt} {
			if err := s.CreateEnvironment(ctx, e); err != nil {
				t.Fatal(err)
			}
		}

		// Nothing configured: no sweep anywhere. Never inventing a default matters —
		// the default would delete things on a host nobody asked about.
		got, err := s.EffectiveCleanupPolicy(ctx, env.ID)
		if err != nil || got != nil {
			t.Fatalf("EffectiveCleanupPolicy with nothing set = (%v, %v); want (nil, nil)", got, err)
		}

		global := &CleanupPolicy{Enabled: true, Schedule: "30 3 * * *", KeepHours: 168,
			Targets: []CleanupTarget{CleanupImages, CleanupContainers}}
		if err := s.SaveGlobalCleanupPolicy(ctx, global); err != nil {
			t.Fatal(err)
		}

		// Both hosts follow the fleet default until one of them overrides.
		for _, id := range []string{env.ID, exempt.ID} {
			p, err := s.EffectiveCleanupPolicy(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if p == nil || !p.Enabled || p.Schedule != "30 3 * * *" || p.KeepHours != 168 {
				t.Fatalf("host %s does not follow the fleet default: %+v", id, p)
			}
		}

		// A DISABLED override exempts its host and leaves the other one swept.
		if err := s.SaveEnvCleanupPolicy(ctx, exempt.ID, &CleanupPolicy{Enabled: false}); err != nil {
			t.Fatal(err)
		}
		p, err := s.EffectiveCleanupPolicy(ctx, exempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if p == nil || p.Enabled {
			t.Errorf("a disabled override did not exempt the host: %+v", p)
		}
		if p, err := s.EffectiveCleanupPolicy(ctx, env.ID); err != nil || p == nil || !p.Enabled {
			t.Errorf("the other host stopped following the fleet default: %+v (%v)", p, err)
		}

		// Reverting drops back to the fleet default rather than to "no cleanup".
		if err := s.DeleteEnvCleanupPolicy(ctx, exempt.ID); err != nil {
			t.Fatal(err)
		}
		if p, err := s.EffectiveCleanupPolicy(ctx, exempt.ID); err != nil || p == nil || !p.Enabled {
			t.Errorf("reverting did not restore the fleet default: %+v (%v)", p, err)
		}
	})
}

// Targets round-trip through the space-separated column in RUN order, not the order they
// were typed: containers before images, because a stopped container pins its image and
// pruning it first is what lets the image go in the same pass.
func TestCleanupPolicyTargetsRoundTripInRunOrder(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		p := &CleanupPolicy{Enabled: true, Schedule: "0 4 * * *", KeepHours: 72,
			Targets: []CleanupTarget{CleanupBuildCache, CleanupImages, CleanupContainers}}
		if err := s.SaveGlobalCleanupPolicy(ctx, p); err != nil {
			t.Fatal(err)
		}
		got, err := s.GlobalCleanupPolicy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := []CleanupTarget{CleanupContainers, CleanupImages, CleanupBuildCache}
		if len(got.Targets) != len(want) {
			t.Fatalf("Targets = %v; want %v", got.Targets, want)
		}
		for i := range want {
			if got.Targets[i] != want[i] {
				t.Errorf("Targets = %v; want %v", got.Targets, want)
				break
			}
		}
		if got.KeepHours != 72 || got.Schedule != "0 4 * * *" || !got.Enabled {
			t.Errorf("policy did not round-trip: %+v", got)
		}
		if got.UpdatedAt.IsZero() {
			t.Error("UpdatedAt was not stamped")
		}

		// Clearing is idempotent and really does unset.
		if err := s.DeleteGlobalCleanupPolicy(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteGlobalCleanupPolicy(ctx); err != nil {
			t.Errorf("clearing an already-clear policy failed: %v", err)
		}
		if got, err := s.GlobalCleanupPolicy(ctx); err != nil || got != nil {
			t.Errorf("policy survived the delete: %+v (%v)", got, err)
		}
	})
}

// Every refusal is a 400 the API can print verbatim, so each one is checked for the
// sentinel rather than just for "an error". Volumes get their own case: a scheduled volume
// prune is deleted DATA, and the refusal has to say so rather than quietly dropping the
// target from the list.
func TestCleanupPolicyValidation(t *testing.T) {
	bad := []struct {
		name string
		p    CleanupPolicy
	}{
		{"volumes are never automatic", CleanupPolicy{Enabled: true, Schedule: "0 3 * * *",
			Targets: []CleanupTarget{"volumes"}}},
		{"unknown target", CleanupPolicy{Enabled: true, Schedule: "0 3 * * *",
			Targets: []CleanupTarget{"everything"}}},
		{"duplicate target", CleanupPolicy{Enabled: true, Schedule: "0 3 * * *",
			Targets: []CleanupTarget{CleanupImages, CleanupImages}}},
		{"enabled without a schedule", CleanupPolicy{Enabled: true,
			Targets: []CleanupTarget{CleanupImages}}},
		{"enabled with nothing to prune", CleanupPolicy{Enabled: true, Schedule: "0 3 * * *"}},
		{"negative keep hours", CleanupPolicy{KeepHours: -1}},
		{"keep hours past a year", CleanupPolicy{KeepHours: maxKeepHours + 1}},
	}
	for _, c := range bad {
		err := c.p.Validate()
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !errors.Is(err, ErrInvalidCleanupPolicy) {
			t.Errorf("%s: error %v does not match the sentinel, so the API answers 500 instead of 400", c.name, err)
		}
	}

	// A DISABLED policy is only shape-checked: switching the sweep off must not first
	// require fixing the schedule being switched off.
	off := CleanupPolicy{Enabled: false, Schedule: "", Targets: nil}
	if err := off.Validate(); err != nil {
		t.Errorf("a disabled policy was refused: %v", err)
	}
}

// The last-run record is what tells an operator the sweep is still happening. A run that
// started and never finished must NOT read as the last successful one.
func TestCleanupRunRecordsInFlightAndOutcome(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		env := &Environment{Name: "prod"}
		if err := s.CreateEnvironment(ctx, env); err != nil {
			t.Fatal(err)
		}
		if got, err := s.LastCleanupRun(ctx, env.ID); err != nil || got != nil {
			t.Fatalf("a never-swept host reported a run: %+v (%v)", got, err)
		}

		if err := s.StartCleanupRun(ctx, env.ID, "schedule"); err != nil {
			t.Fatal(err)
		}
		got, err := s.LastCleanupRun(ctx, env.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.EndedAt != nil {
			t.Error("an in-flight run reported an end time")
		}
		if err := s.FinishCleanupRun(ctx, env.ID, 4_200_000_000, 17, nil); err != nil {
			t.Fatal(err)
		}
		if got, err = s.LastCleanupRun(ctx, env.ID); err != nil {
			t.Fatal(err)
		}
		if got.EndedAt == nil || got.Freed != 4_200_000_000 || got.Deleted != 17 || got.Error != "" {
			t.Errorf("finished run = %+v", got)
		}

		// The next run replaces the previous one INCLUDING its numbers: a sweep that
		// failed must not still be showing the 4GB the last good one reclaimed.
		if err := s.StartCleanupRun(ctx, env.ID, "manual"); err != nil {
			t.Fatal(err)
		}
		if got, err = s.LastCleanupRun(ctx, env.ID); err != nil {
			t.Fatal(err)
		}
		if got.Freed != 0 || got.Deleted != 0 || got.Trigger != "manual" || got.EndedAt != nil {
			t.Errorf("a new run did not reset the previous outcome: %+v", got)
		}
		if err := s.FinishCleanupRun(ctx, env.ID, 0, 0, errors.New("the daemon went away")); err != nil {
			t.Fatal(err)
		}
		if got, err = s.LastCleanupRun(ctx, env.ID); err != nil {
			t.Fatal(err)
		}
		if got.Error != "the daemon went away" {
			t.Errorf("the failure was not recorded: %+v", got)
		}
	})
}

// 0016 adds three tables to a POPULATED database, and the per-host ones carry an env_id FK
// that must accept the environments already in it. Fresh-schema tests cannot see this:
// they create the environment after the migration. SQLite-only — the stopAfter seam is a
// package var, so this test controls the schema version directly.
func TestMigrate0016CleanupOnPopulatedDatabase(t *testing.T) {
	ctx := context.Background()
	url := "sqlite://" + filepath.Join(t.TempDir(), "test.db")

	stopAfter = "0015_fleet_deliveries"
	defer func() { stopAfter = "" }()

	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open at 0015: %v", err)
	}
	defer s.Close()

	env := &Environment{Name: "prod"}
	if err := s.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}

	stopAfter = ""
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrating to 0016: %v", err)
	}

	// The pre-existing host has no policy and no run — the migration invents neither.
	if p, err := s.EffectiveCleanupPolicy(ctx, env.ID); err != nil || p != nil {
		t.Errorf("the migration invented a policy for an existing host: %+v (%v)", p, err)
	}
	if r, err := s.LastCleanupRun(ctx, env.ID); err != nil || r != nil {
		t.Errorf("the migration invented a run for an existing host: %+v (%v)", r, err)
	}

	// And the FK accepts it: a policy and a run attach to the host that predates the tables.
	if err := s.SaveEnvCleanupPolicy(ctx, env.ID, &CleanupPolicy{Enabled: true, Schedule: "0 3 * * *",
		KeepHours: 168, Targets: []CleanupTarget{CleanupImages}}); err != nil {
		t.Fatalf("attaching a policy to a pre-existing host: %v", err)
	}
	if err := s.StartCleanupRun(ctx, env.ID, "manual"); err != nil {
		t.Fatalf("recording a run for a pre-existing host: %v", err)
	}
}
