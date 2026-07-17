package api

// The generator's guard rails. Four properties are enforced here:
//
//  1. The checked-in artifacts are CURRENT — rebuilt in memory and byte-compared, so a
//     table edit that skips `go generate ./internal/api` fails in CI, not in a client.
//  2. Every //oapi:example actually validates against the schema it illustrates.
//  3. Coverage only ratchets FORWARD: the list of routes without declared types may
//     shrink (run with -update after backfilling) but never grow.
//  4. The generated/manual boundary is disjoint — a name on both sides would let the
//     spread in api.ts silently shadow the handwritten method.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var updateGoldens = flag.Bool("update", false, "rewrite testdata ratchet goldens")

const (
	undocGolden  = "testdata/undocumented_routes.txt"
	manualGolden = "testdata/manual_client.txt"
	manualTS     = "../../web/src/lib/api-manual.ts"
	generatedTS  = "../../web/src/lib/api.ts"
)

func TestGeneratedArtifactsAreCurrent(t *testing.T) {
	spec, client, err := GenerateArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if string(spec) != string(openAPISpec) {
		t.Error("openapi.json is stale — run: go generate ./internal/api")
	}
	onDisk, err := os.ReadFile(generatedTS)
	if err != nil {
		t.Fatal(err)
	}
	if client != string(onDisk) {
		t.Error("web/src/lib/api.ts is stale — run: go generate ./internal/api")
	}
}

// Every component must compile as a JSON schema, and every example must validate against
// the schema it illustrates. This is the in-repo replacement for an external OpenAPI
// linter: a schema that does not compile, or an example that lies, fails here.
func TestOpenAPIExamplesValidate(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			RequestBody *struct {
				Content map[string]struct {
					Schema  json.RawMessage `json:"schema"`
					Example json.RawMessage `json:"example"`
				} `json:"content"`
			} `json:"requestBody"`
			Responses map[string]struct {
				Content map[string]struct {
					Schema  json.RawMessage `json:"schema"`
					Example json.RawMessage `json:"example"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &doc); err != nil {
		t.Fatal(err)
	}

	// One resource bundle holding every component, so $refs resolve.
	root := map[string]any{"components": map[string]any{"schemas": map[string]any{}}}
	schemas := root["components"].(map[string]any)["schemas"].(map[string]any)
	for name, raw := range doc.Components.Schemas {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		schemas[name] = v
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("spec.json", root); err != nil {
		t.Fatal(err)
	}

	// Compiling every component proves each is a well-formed schema.
	for name := range doc.Components.Schemas {
		if _, err := compiler.Compile("spec.json#/components/schemas/" + name); err != nil {
			t.Errorf("component %s does not compile: %v", name, err)
		}
	}

	validate := func(op, where string, schema, example json.RawMessage) {
		if len(example) == 0 || len(schema) == 0 {
			return
		}
		// Anchor the example's schema inside the bundle so $refs keep resolving. Examples
		// only make sense on named components anyway — an inline schema with an example
		// is a smell this test turns into an error.
		var ref struct {
			Ref string `json:"$ref"`
		}
		if err := json.Unmarshal(schema, &ref); err != nil || ref.Ref == "" {
			t.Errorf("%s %s: example on an inline schema — name the type so the example has a schema to validate against", op, where)
			return
		}
		loc := "spec.json" + ref.Ref // "#/components/schemas/X" appended to the bundle id
		compiled, err := compiler.Compile(loc)
		if err != nil {
			t.Errorf("%s %s: schema %s does not compile: %v", op, where, loc, err)
			return
		}
		var value any
		if err := json.Unmarshal(example, &value); err != nil {
			t.Errorf("%s %s: example is not JSON: %v", op, where, err)
			return
		}
		if err := compiled.Validate(value); err != nil {
			t.Errorf("%s %s: the example does not match its own schema:\n%v", op, where, err)
		}
	}

	for _, methods := range doc.Paths {
		for _, op := range methods {
			if op.RequestBody != nil {
				for _, c := range op.RequestBody.Content {
					validate(op.OperationID, "request example", c.Schema, c.Example)
				}
			}
			for status, resp := range op.Responses {
				for _, c := range resp.Content {
					validate(op.OperationID, "response "+status+" example", c.Schema, c.Example)
				}
			}
		}
	}
}

// The coverage ratchet. Routes without declared payload types are pinned in a golden
// that may only SHRINK: backfill a route, run `go test ./internal/api -update`, and the
// diff shows exactly what got documented. A new route must arrive already declared or be
// added to the golden by hand — a decision, not a drift.
func TestOpenAPICoverageOnlyRatchetsForward(t *testing.T) {
	metas, err := RouteMetas()
	if err != nil {
		t.Fatal(err)
	}
	docs, err := ParseRouteDocs("server.go")
	if err != nil {
		t.Fatal(err)
	}

	var undocumented []string
	for _, m := range metas {
		doc := docs.ByPattern[m.Pattern]
		bodied := m.Method == "POST" || m.Method == "PUT" || m.Method == "PATCH"
		covered := (m.Resp != nil || doc.Status == 204 || doc.Produces != "") &&
			(!bodied || m.Req != nil || doc.Status == 204 || doc.NoReq)
		if !covered {
			undocumented = append(undocumented, m.Pattern)
		}
	}

	ratchet(t, undocGolden, undocumented,
		"a route lost its type declarations — the spec only moves forward")
}

// The manual-client ratchet: api-manual.ts may only shrink. Growth means somebody hand-added to
// the file that exists to be emptied.
func TestManualClientOnlyShrinks(t *testing.T) {
	manual, err := os.ReadFile(manualTS)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range tsMethodRe.FindAllStringSubmatch(string(manual), -1) {
		names = append(names, "method "+m[1])
	}
	for _, m := range tsTypeRe.FindAllStringSubmatch(string(manual), -1) {
		names = append(names, "type "+m[1])
	}
	ratchet(t, manualGolden, names,
		"api-manual.ts grew — new client code belongs in the route table, not the manual file")
}

func TestGeneratedAndManualAreDisjoint(t *testing.T) {
	manual, err := os.ReadFile(manualTS)
	if err != nil {
		t.Fatal(err)
	}
	_, client, err := GenerateArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckManualDisjoint(string(manual), client); err != nil {
		t.Fatal(err)
	}
}

// ratchet asserts current ⊆ golden, and -update rewrites the golden to current (which
// the assertion then makes monotone: you can only ever remove entries).
func ratchet(t *testing.T, golden string, current []string, grewMsg string) {
	t.Helper()
	if *updateGoldens {
		if err := os.WriteFile(golden, []byte(strings.Join(current, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%s missing — run: go test ./internal/api -run %s -update", golden, t.Name())
	}
	allowed := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			allowed[line] = true
		}
	}
	for _, entry := range current {
		if !allowed[entry] {
			t.Errorf("%s: %q", grewMsg, entry)
		}
	}
}

// The generated file must at least PARSE as TypeScript. vue-tsc remains the real
// typecheck (pnpm build); this catches a mangled emission without a web toolchain run.
// The caps_test.go treatment: skip without node, lean on web/node_modules.
func TestGeneratedClientParsesAsTypeScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	tsLib, err := filepath.Abs("../../web/node_modules/typescript")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tsLib); err != nil {
		t.Skip("web/node_modules/typescript not installed — run pnpm install")
	}
	src, err := filepath.Abs(generatedTS)
	if err != nil {
		t.Fatal(err)
	}

	js := fmt.Sprintf(`
		const ts = require(%q)
		const fs = require('fs')
		const text = fs.readFileSync(%q, 'utf8')
		const sf = ts.createSourceFile('api.ts', text, ts.ScriptTarget.ES2022, true, ts.ScriptKind.TS)
		const bad = sf.parseDiagnostics
		if (bad.length) {
			for (const d of bad.slice(0, 5)) {
				const { line } = sf.getLineAndCharacterOfPosition(d.start)
				console.error('line ' + (line + 1) + ': ' + ts.flattenDiagnosticMessageText(d.messageText, ' '))
			}
			process.exit(1)
		}
	`, tsLib, src)
	out, err := exec.Command(node, "-e", js).CombinedOutput()
	if err != nil {
		t.Fatalf("generated api.ts does not parse:\n%s", out)
	}
}
