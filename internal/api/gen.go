//go:build ignore

// gen.go regenerates the two artifacts the route table is the source of truth for:
//
//	internal/api/openapi.json   — the OpenAPI 3.1 description of the whole API
//	web/src/lib/api.ts          — the typed browser client, fully generated
//
// Run via: go generate ./internal/api
//
// It refuses to run when a generated method or interface name still exists in
// api-manual.ts: the generated daffa spreads OVER manualDaffa, so a collision would
// silently shadow the handwritten method instead of failing the migration commit.
package main

import (
	"fmt"
	"os"
	"regexp"

	"github.com/Mnshahawy/daffa/internal/api"
	"github.com/Mnshahawy/daffa/internal/openapi"
)

const manualPath = "../../web/src/lib/api-manual.ts"

func main() {
	spec, client, err := api.GenerateArtifacts()
	if err != nil {
		fatal(err)
	}

	manual, err := os.ReadFile(manualPath)
	if err != nil {
		fatal(fmt.Errorf("reading %s: %w", manualPath, err))
	}
	if err := api.CheckManualDisjoint(string(manual), client); err != nil {
		fatal(err)
	}

	if err := os.WriteFile("openapi.json", spec, 0o644); err != nil {
		fatal(err)
	}
	if err := os.WriteFile("../../web/src/lib/api.ts", []byte(client), 0o644); err != nil {
		fatal(err)
	}
	ops := regexp.MustCompile(`"operationId"`).FindAllIndex(spec, -1)
	fmt.Printf("openapi: wrote openapi.json (%d operations) and web/src/lib/api.ts\n", len(ops))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gen:", err)
	os.Exit(1)
}

var _ = openapi.Doc{} // the real work lives in api.GenerateArtifacts, beside the table
