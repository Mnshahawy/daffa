package api

import (
	"testing"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The store names cleanup targets in its own strings (it does not know what Docker is) and
// cleanupPruneTarget is the single place the two vocabularies meet. A target a policy can
// hold but the sweep cannot map is silently skipped at 03:30 — the sweep reports success
// having done nothing — so the mapping is total, and pinned here.
func TestCleanupTargetsAllMapToPrunes(t *testing.T) {
	for _, target := range store.CleanupTargets {
		pt, ok := cleanupPruneTarget(target)
		if !ok {
			t.Errorf("policy target %q maps to no prune, so a policy naming it would silently do nothing", target)
			continue
		}
		if !dockerx.ValidPruneTarget(pt) {
			t.Errorf("policy target %q maps to %q, which is not a prune target Docker knows", target, pt)
		}
		if pt == dockerx.PruneVolumes {
			t.Errorf("policy target %q maps to the VOLUME prune — an automatic sweep must never delete data", target)
		}
	}

	// The converse: volumes are a valid manual prune and must stay unreachable from a
	// policy. store.CleanupTargets is the allowlist; this proves it did not grow one.
	if store.ValidCleanupTarget("volumes") {
		t.Error("volumes became a policy target — a scheduled volume prune is deleted data, not a re-pull")
	}
}
