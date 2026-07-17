package store

import (
	"context"
	"errors"
	"testing"
)

// Container logging defaults are a fixed-id global singleton plus per-host overrides. This
// covers precedence (global applies until a host overrides and returns when it reverts), the
// UPSERT on a second save, validation via the shared sentinel, and the env cascade.
func TestLogConfigPrecedenceAndValidation(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		staging := &Environment{Name: "staging"}
		if err := s.CreateEnvironment(ctx, staging); err != nil {
			t.Fatal(err)
		}

		// Unset is a normal state, not an error — and it means "inject nothing".
		if c, err := s.EffectiveLogConfig(ctx, staging.ID); err != nil || c != nil {
			t.Fatalf("EffectiveLogConfig on a fresh install = (%v, %v); want (nil, nil)", c, err)
		}

		// Global round-trip, and a second save must UPSERT rather than duplicate.
		g := &LogConfig{Driver: "json-file", Opts: map[string]string{"max-size": "10m", "max-file": "3"}}
		if err := s.SaveGlobalLogConfig(ctx, g); err != nil {
			t.Fatal(err)
		}
		if g.UpdatedAt.IsZero() {
			t.Error("SaveGlobalLogConfig did not stamp UpdatedAt back onto the struct")
		}
		g.Opts["max-size"] = "5m"
		if err := s.SaveGlobalLogConfig(ctx, g); err != nil {
			t.Fatal(err)
		}
		got, err := s.GlobalLogConfig(ctx)
		if err != nil || got == nil || got.Driver != "json-file" || got.Opts["max-size"] != "5m" {
			t.Fatalf("GlobalLogConfig after two saves = (%+v, %v); want the updated single row", got, err)
		}

		// Precedence: global applies until the host overrides, and returns when it reverts.
		if c, _ := s.EffectiveLogConfig(ctx, staging.ID); c == nil || c.Driver != "json-file" {
			t.Fatalf("effective config without an override = %+v; want the global default", c)
		}
		ov := &LogConfig{Driver: "local", Opts: map[string]string{"max-size": "20m"}}
		if err := s.SaveEnvLogConfig(ctx, staging.ID, ov); err != nil {
			t.Fatal(err)
		}
		if c, _ := s.EffectiveLogConfig(ctx, staging.ID); c == nil || c.Driver != "local" {
			t.Fatalf("effective config with an override = %+v; want the override", c)
		}
		if err := s.DeleteEnvLogConfig(ctx, staging.ID); err != nil {
			t.Fatal(err)
		}
		if c, _ := s.EffectiveLogConfig(ctx, staging.ID); c == nil || c.Driver != "json-file" {
			t.Fatalf("effective config after revert = %+v; want the global default back", c)
		}
		// Deleting what is already gone is the state the caller wanted, not an error.
		if err := s.DeleteEnvLogConfig(ctx, staging.ID); err != nil {
			t.Fatalf("a second delete must be a no-op: %v", err)
		}

		// Every invalid shape shares the sentinel — that is what buys the API its 400.
		for _, bad := range []*LogConfig{
			{Driver: ""},
			{Driver: "json file"},
			{Driver: "json-file", Opts: map[string]string{"max size": "1m"}},
			{Driver: "json-file", Opts: map[string]string{"max-size": "1m\nmax-file: 3"}},
		} {
			if err := s.SaveGlobalLogConfig(ctx, bad); !errors.Is(err, ErrInvalidLogConfig) {
				t.Errorf("saving %+v = %v; want ErrInvalidLogConfig", bad, err)
			}
		}

		// Deleting the environment cascades its override away; the global row is not the
		// environment's and must survive.
		if err := s.SaveEnvLogConfig(ctx, staging.ID, ov); err != nil {
			t.Fatal(err)
		}
		if _, err := s.exec(ctx, `DELETE FROM environments WHERE id = ?`, staging.ID); err != nil {
			t.Fatal(err)
		}
		if c, _ := s.EnvLogConfig(ctx, staging.ID); c != nil {
			t.Errorf("an override survived its environment's deletion: %+v", c)
		}
		if c, _ := s.GlobalLogConfig(ctx); c == nil {
			t.Error("the global default must outlive any environment")
		}
	})
}
