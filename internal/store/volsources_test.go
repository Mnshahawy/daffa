package store

import (
	"context"
	"errors"
	"testing"
)

func TestVolumeSourceRoundTrip(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		prod, staging := twoHosts(t, s)

		src := &VolumeSource{
			EnvID: prod.ID, Volume: "traefik-dynamic",
			GitURL: "https://example.com/infra.git", GitRef: "main", GitPath: "traefik/dynamic",
			UID: 100, GID: 100, RestartTargets: "traefik",
		}
		if err := s.CreateVolumeSource(ctx, src); err != nil {
			t.Fatal(err)
		}
		if src.Status != "pending" {
			t.Errorf("a new source starts %q, want pending", src.Status)
		}

		// List filters by env, never gates.
		both, err := s.ListVolumeSources(ctx, true, nil)
		if err != nil || len(both) != 1 {
			t.Fatalf("global list: %v, %v", both, err)
		}
		none, err := s.ListVolumeSources(ctx, false, []string{staging.ID})
		if err != nil || len(none) != 0 {
			t.Fatalf("staging-only list must not see prod's source: %v, %v", none, err)
		}

		// A sync outcome lands on the row.
		if err := s.MarkVolumeSourceSynced(ctx, src.ID, "hash1", "abc123", "cfg/leaked looks like a key", nil); err != nil {
			t.Fatal(err)
		}
		got, err := s.VolumeSourceByID(ctx, src.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "ok" || got.SyncedHash != "hash1" || got.SyncedCommit != "abc123" ||
			got.Warnings == "" || got.SyncedAt.IsZero() {
			t.Errorf("sync outcome not recorded: %+v", got)
		}

		// A failed sync keeps the error verbatim.
		if err := s.MarkVolumeSourceSynced(ctx, src.ID, "hash1", "abc123", "", errors.New("node unreachable")); err != nil {
			t.Fatal(err)
		}
		got, _ = s.VolumeSourceByID(ctx, src.ID)
		if got.Status != "error" || got.LastError != "node unreachable" {
			t.Errorf("failure outcome not recorded: status=%q err=%q", got.Status, got.LastError)
		}

		// Update rewrites the mutable fields only.
		got.GitRef = "v2"
		got.AutoSync = true
		if err := s.UpdateVolumeSource(ctx, got); err != nil {
			t.Fatal(err)
		}
		got, _ = s.VolumeSourceByID(ctx, src.ID)
		if got.GitRef != "v2" || !got.AutoSync {
			t.Errorf("update did not land: %+v", got)
		}

		if err := s.DeleteVolumeSource(ctx, src.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.VolumeSourceByID(ctx, src.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after delete: %v, want ErrNotFound", err)
		}
	})
}

func TestVolumeSourcesByStack(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		prod, _ := twoHosts(t, s)

		stack := &Stack{EnvID: prod.ID, Name: "edge", SourceKind: "inline", InlineYAML: "services: {}"}
		if err := s.CreateStack(ctx, stack); err != nil {
			t.Fatal(err)
		}
		linked := &VolumeSource{EnvID: prod.ID, Volume: "edge-config",
			GitURL: "https://example.com/infra.git", StackID: stack.ID}
		loose := &VolumeSource{EnvID: prod.ID, Volume: "other-config",
			GitURL: "https://example.com/infra.git"}
		for _, v := range []*VolumeSource{linked, loose} {
			if err := s.CreateVolumeSource(ctx, v); err != nil {
				t.Fatal(err)
			}
		}

		got, err := s.VolumeSourcesByStack(ctx, stack.ID)
		if err != nil || len(got) != 1 || got[0].ID != linked.ID {
			t.Fatalf("VolumeSourcesByStack = %v, %v; want just the linked source", got, err)
		}

		// Deleting the stack unlinks the source — it must NOT delete it. The volume (and
		// whatever mounts it next) may outlive the stack that introduced it.
		if err := s.DeleteStack(ctx, stack.ID); err != nil {
			t.Fatal(err)
		}
		survivor, err := s.VolumeSourceByID(ctx, linked.ID)
		if err != nil {
			t.Fatalf("the source must survive its stack: %v", err)
		}
		if survivor.StackID != "" {
			t.Errorf("stack_id must null out, got %q", survivor.StackID)
		}
	})
}
