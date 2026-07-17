package store

import (
	"context"
	"os"
	"testing"

	"github.com/Mnshahawy/daffa/internal/caps"
)

// A capability mask must be able to hold a HIGH BIT on Postgres, and this test is Postgres-only
// because that is the entire point.
//
// SQLite's INTEGER is 64-bit, so the SQLite path cannot notice a column that is too narrow and
// never will. Postgres's INTEGER is 32-bit and SIGNED, so the highest bit role_caps.mask can
// carry is caps.MaxBit (30). Masks are deliberately small — one INTEGER per namespace, cached in
// memory for every user — and the ceiling is now close enough that only this test would catch a
// column quietly widened past what the code assumes, or narrowed below what a full namespace
// needs, until an area filled up and one particular grant started failing on one dialect.
//
// It writes the bit through raw SQL rather than the store, because the store's Normalize would
// (correctly) discard a bit that no capability owns. The question here is what the COLUMN can
// hold, not what the registry knows.
func TestAMaskColumnHoldsAHighBitOnPostgres(t *testing.T) {
	url := os.Getenv("DAFFA_TEST_PG_URL")
	if url == "" {
		t.Skip("DAFFA_TEST_PG_URL not set — the mask column's width is NOT covered by this run")
	}
	ctx := context.Background()

	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t.Cleanup(func() {
		_, _ = s.db.Exec("DROP SCHEMA IF EXISTS " + quoteIdent(s.pgSchema) + " CASCADE")
	})

	// Bit 30 — caps.MaxBit, the highest an area may ever use and still round-trip through a
	// signed 32-bit INTEGER.
	const high = int64(1) << caps.MaxBit

	r := &Role{Name: "Wide", Description: "holds a high bit"}
	if err := s.CreateRole(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO role_caps (role_id, ns, mask) VALUES (?, 'docker', ?)`), r.ID, high); err != nil {
		t.Fatalf("role_caps.mask cannot hold bit %d: %v\n\n"+
			"The column is too narrow for a full namespace — an administrator granting a "+
			"capability in that position would be told \"integer out of range\", and only on "+
			"Postgres.", caps.MaxBit, err)
	}

	var got int64
	if err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT mask FROM role_caps WHERE role_id = ? AND ns = 'docker'`), r.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != high {
		t.Errorf("bit %d did not survive the round trip: stored %d, read back %d", caps.MaxBit, high, got)
	}
}
