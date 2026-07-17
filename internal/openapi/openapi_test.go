package openapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// One fixture family exercises every reflector rule, and each case asserts the schema
// JSON and the TS text from the SAME walk — the two emitters share the registry, so a
// drift between them can only be a bug here, and this is where it fails.

type fixtureNested struct {
	Kind string `json:"kind"`
}

type fixture struct {
	Name      string            `json:"name"`
	Count     int               `json:"count"`
	Ratio     float64           `json:"ratio"`
	Enabled   bool              `json:"enabled"`
	When      time.Time         `json:"when"`
	MaybeAt   *time.Time        `json:"maybe_at"`
	Tags      []string          `json:"tags,omitempty"`
	Opts      map[string]string `json:"opts"`
	Blob      []byte            `json:"blob"`
	Loose     map[string]any    `json:"loose"`
	Anything  any               `json:"anything"`
	Nested    fixtureNested     `json:"nested"`
	MaybeSub  *fixtureNested    `json:"maybe_sub"`
	AsString  int               `json:"as_string,string"`
	Hidden    string            `json:"-"`
	unexp     string            //nolint:unused
	Anon      struct {
		X int `json:"x"`
	} `json:"anon"`
}

type fixtureEmbed struct {
	fixtureNested
	Own string `json:"own"`
}

type fixtureCycle struct {
	Next *fixtureCycle `json:"next"`
}

func mustAdd(t *testing.T, r *Reflector, v any, mode Mode, nullable bool) *Schema {
	t.Helper()
	s, err := r.Add(reflect.TypeOf(v), mode, nullable)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func schemaText(t *testing.T, s *Schema) string {
	t.Helper()
	b, err := json.Marshal(schemaJSON(s))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func componentByName(t *testing.T, r *Reflector, name string) *Component {
	t.Helper()
	for _, c := range r.Components() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("component %q was not registered; have %v", name, names(r))
	return nil
}

func names(r *Reflector) []string {
	var out []string
	for _, c := range r.Components() {
		out = append(out, c.Name)
	}
	return out
}

func TestReflectorFieldRules(t *testing.T) {
	r := NewReflector(nil)
	top := mustAdd(t, r, fixture{}, Response, false)

	if top.Ref != "#/components/schemas/Fixture" {
		t.Fatalf("top-level struct should be a component ref, got %+v", top)
	}
	c := componentByName(t, r, "Fixture")

	// Field-by-field expectations: JSON schema fragment and TS type, from one walk.
	// encoding/json semantics, exactly: a pointer WITHOUT omitempty is emitted as null
	// (present, nullable); omitempty means absent (optional), never null.
	cases := map[string]struct {
		schema   string
		ts       string
		optional bool
	}{
		"name":      {`{"type":"string"}`, "string", false},
		"count":     {`{"type":"integer"}`, "number", false},
		"ratio":     {`{"type":"number"}`, "number", false},
		"enabled":   {`{"type":"boolean"}`, "boolean", false},
		"when":      {`{"type":"string","format":"date-time"}`, "string", false},
		"maybe_at":  {`{"oneOf":[{"type":"string","format":"date-time"},{"type":"null"}]}`, "string | null", false},
		"tags":      {`{"type":"array","items":{"type":"string"}}`, "string[]", true},
		"opts":      {`{"type":"object","additionalProperties":{"type":"string"}}`, "Record<string, string>", false},
		"blob":      {`{"type":"string","contentEncoding":"base64"}`, "string", false},
		"loose":     {`{"type":"object"}`, "Record<string, unknown>", false},
		"anything":  {`{}`, "unknown", false},
		"nested":    {`{"$ref":"#/components/schemas/FixtureNested"}`, "FixtureNested", false},
		"maybe_sub": {`{"oneOf":[{"$ref":"#/components/schemas/FixtureNested"},{"type":"null"}]}`, "FixtureNested | null", false},
		"as_string": {`{"type":"string"}`, "string", false},
		"anon":      {`{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}`, "{ x: number }", false},
	}

	got := map[string]Property{}
	for _, p := range c.Schema.Properties {
		got[p.Name] = p
	}
	if _, ok := got["Hidden"]; ok {
		t.Error(`json:"-" field leaked into the schema`)
	}
	if _, ok := got["unexp"]; ok {
		t.Error("unexported field leaked into the schema")
	}
	for name, want := range cases {
		p, ok := got[name]
		if !ok {
			t.Errorf("property %q missing", name)
			continue
		}
		if s := schemaText(t, p.Schema); s != want.schema {
			t.Errorf("%s schema = %s; want %s", name, s, want.schema)
		}
		if ts := tsType(p.Schema); ts != want.ts {
			t.Errorf("%s ts = %s; want %s", name, ts, want.ts)
		}
		if optional := !c.Schema.IsRequired(name); optional != want.optional {
			t.Errorf("%s optional = %v; want %v", name, optional, want.optional)
		}
	}
}

func TestReflectorRequestMode(t *testing.T) {
	r := NewReflector(nil)
	mustAdd(t, r, fixtureNested{}, Request, false)
	c := componentByName(t, r, "FixtureNestedRequest")

	if len(c.Schema.Required) != 0 {
		t.Errorf("request fields default to optional (Decode reads absent as zero); required = %v", c.Schema.Required)
	}
	if !c.Schema.Closed {
		t.Error("request components must carry additionalProperties:false — httpx.Decode refuses unknown fields")
	}
	if err := r.SetRequired("FixtureNestedRequest", "kind", true); err != nil {
		t.Fatal(err)
	}
	if len(c.Schema.Required) != 1 || c.Schema.Required[0] != "kind" {
		t.Errorf("//oapi:required did not apply: %v", c.Schema.Required)
	}
}

func TestReflectorDualUseAndOverrides(t *testing.T) {
	r := NewReflector(map[reflect.Type]string{reflect.TypeOf(fixtureNested{}): "Renamed"})
	mustAdd(t, r, fixtureNested{}, Response, false)
	mustAdd(t, r, fixtureNested{}, Request, false)

	if componentByName(t, r, "Renamed") == nil || componentByName(t, r, "RenamedRequest") == nil {
		t.Fatal("dual-use type should yield both Renamed and RenamedRequest")
	}

	// Two different types wanting one name is a loud error, not a silent merge.
	r2 := NewReflector(map[reflect.Type]string{
		reflect.TypeOf(fixture{}):       "Same",
		reflect.TypeOf(fixtureNested{}): "Same",
	})
	mustAdd(t, r2, fixtureNested{}, Response, false)
	if _, err := r2.Add(reflect.TypeOf(fixture{}), Response, false); err == nil {
		t.Fatal("a component-name collision between two types must refuse to generate")
	}
}

func TestReflectorEmbeddedAndCycles(t *testing.T) {
	r := NewReflector(nil)
	mustAdd(t, r, fixtureEmbed{}, Response, false)
	c := componentByName(t, r, "FixtureEmbed")
	var propNames []string
	for _, p := range c.Schema.Properties {
		propNames = append(propNames, p.Name)
	}
	if strings.Join(propNames, ",") != "kind,own" {
		t.Errorf("embedded promotion produced %v; want [kind own]", propNames)
	}

	mustAdd(t, r, fixtureCycle{}, Response, false)
	cy := componentByName(t, r, "FixtureCycle")
	next := cy.Schema.Properties[0].Schema
	if len(next.OneOf) == 0 || next.OneOf[0].Ref != "#/components/schemas/FixtureCycle" {
		t.Errorf("cycle must terminate at a self-ref: %+v", next)
	}
}

func TestReflectorNullableTopLevelAndEnums(t *testing.T) {
	r := NewReflector(nil)
	s := mustAdd(t, r, (*fixtureNested)(nil), Response, true)
	if tsType(s) != "FixtureNested | null" {
		t.Errorf("nullable top-level = %s; want FixtureNested | null", tsType(s))
	}

	if err := r.SetEnum("FixtureNested", "kind", []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	c := componentByName(t, r, "FixtureNested")
	if tsType(c.Schema.Properties[0].Schema) != "'a' | 'b'" {
		t.Errorf("enum ts = %s", tsType(c.Schema.Properties[0].Schema))
	}
	if err := r.SetEnum("FixtureNested", "nope", []string{"x"}); err == nil {
		t.Error("enum on an unknown property must be a loud error")
	}
	if err := r.SetEnum("FixtureNested", "kind", []string{"different"}); err == nil {
		t.Error("conflicting enum sets must be a loud error")
	}
}

// ── spec assembly ─────────────────────────────────────────────────────────────────

func specOps(t *testing.T, r *Reflector) []Op {
	t.Helper()
	req := mustAdd(t, r, fixtureNested{}, Request, false)
	resp := mustAdd(t, r, fixtureNested{}, Response, false)
	return []Op{
		{
			Method: "GET", Path: "/api/things", Pattern: "GET /api/things",
			OperationID: "things", Tag: "things", Summary: "List things",
			Cap: "things.view", Scope: "any", SecurityNote: "Filtered to what the caller may see.",
			Resp: &Schema{Type: "array", Items: resp}, RespDeclared: true, TSName: "things",
		},
		{
			Method: "PUT", Path: "/api/things/{id}", Pattern: "PUT /api/things/{id}",
			OperationID: "saveThing", Tag: "things",
			Cap: "things.edit", Scope: "global",
			Req: req, Resp: resp, RespDeclared: true, TSName: "saveThing",
			ReqExample: json.RawMessage(`{"kind":"a"}`),
		},
		{
			Method: "DELETE", Path: "/api/things/{id}", Pattern: "DELETE /api/things/{id}",
			OperationID: "deleteThing", Tag: "things",
			Cap: "things.edit", Scope: "global",
			Status: 204, RespDeclared: true, TSName: "deleteThing",
		},
	}
}

func TestBuildSpecShapeAndDeterminism(t *testing.T) {
	r := NewReflector(nil)
	ops := specOps(t, r)
	doc := Doc{Title: "Daffa API", Version: "1", Description: "test"}

	one, err := BuildSpec(doc, ops, r.Components())
	if err != nil {
		t.Fatal(err)
	}
	two, err := BuildSpec(doc, ops, r.Components())
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(two) {
		t.Fatal("BuildSpec is not deterministic — the checked-in artifact would churn")
	}

	var parsed map[string]any
	if err := json.Unmarshal(one, &parsed); err != nil {
		t.Fatalf("spec is not valid JSON: %v", err)
	}
	if parsed["openapi"] != "3.1.0" {
		t.Errorf("openapi version = %v", parsed["openapi"])
	}

	text := string(one)
	for _, want := range []string{
		`"x-daffa-capability": "things.view"`,
		`"x-daffa-scope": "any"`,
		`"$ref": "#/components/schemas/ErrorBody"`,
		`"FixtureNestedRequest"`,
		`"additionalProperties": false`,
		`"204"`,
		`"kind": "a"`, // the example payload, re-indented by MarshalIndent
	} {
		if !strings.Contains(text, want) {
			t.Errorf("spec is missing %s", want)
		}
	}
	// A GET has no body, so no 400; the PUT has one.
	if strings.Count(text, `"400"`) != 1 {
		t.Errorf("expected exactly one 400 response (the PUT), got %d", strings.Count(text, `"400"`))
	}
}

// ── TS client emission ────────────────────────────────────────────────────────────

func TestBuildClientMethods(t *testing.T) {
	r := NewReflector(nil)
	ops := specOps(t, r)
	out, err := BuildClient(ops, r.Components())
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"export const daffa = {\n  ...manualDaffa,",
		"things: () => api.get<FixtureNested[]>('/api/things'),",
		"saveThing: (id: string, body: FixtureNestedRequest) =>\n    api.put<FixtureNested>(`/api/things/${id}`, body),",
		"deleteThing: (id: string) => api.del<void>(`/api/things/${id}`),",
		"export interface FixtureNestedRequest {",
		"export * from './api-manual'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("client is missing %q", want)
		}
	}
}

func TestBuildClientRefusals(t *testing.T) {
	// ts: with an undeclared response type — an untyped client method helps nobody.
	_, err := BuildClient([]Op{{
		Method: "GET", Path: "/api/x", Pattern: "GET /api/x",
		OperationID: "x", TSName: "x",
	}}, nil)
	if err == nil {
		t.Fatal("ts without a declared response must refuse to generate")
	}

	// SSE routes keep their handwritten streams.
	_, err = BuildClient([]Op{{
		Method: "GET", Path: "/api/x", Pattern: "GET /api/x",
		OperationID: "x", TSName: "x", Produces: "text/event-stream", RespDeclared: true,
	}}, nil)
	if err == nil {
		t.Fatal("ts on an SSE route must refuse to generate")
	}
}
