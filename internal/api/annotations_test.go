package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRouteDocsFixture(t *testing.T) {
	docs, err := ParseRouteDocs(filepath.Join("testdata", "routes_fixture.go.txt"))
	if err != nil {
		t.Fatal(err)
	}

	list := docs.ByPattern["GET /api/widgets"]
	if list == nil {
		t.Fatal("GET /api/widgets was not parsed")
	}
	if list.Tag != "widgets" {
		t.Errorf("tag = %q; want widgets", list.Tag)
	}
	if list.Summary != "List the widgets" {
		t.Errorf("summary = %q", list.Summary)
	}
	if !strings.Contains(list.Description, "stand-in for a real section") ||
		!strings.Contains(list.Description, "\n\nSecond paragraph") {
		t.Errorf("prose paragraphs mangled: %q", list.Description)
	}
	if string(list.RespExample) != `[{"name": "a"}]` {
		t.Errorf("resp example = %s", list.RespExample)
	}

	create := docs.ByPattern["POST /api/widgets"]
	if create.Status != 201 {
		t.Errorf("status = %d; want 201", create.Status)
	}
	// The multi-line example accumulates until the JSON validates.
	if !strings.Contains(string(create.ReqExample), `"kind": "round"`) {
		t.Errorf("multi-line req example = %s", create.ReqExample)
	}
	// The section prose belongs to the FIRST route; the second starts clean.
	if create.Description != "" {
		t.Errorf("prose leaked across routes: %q", create.Description)
	}

	// Enum + required rules are collected globally with their declaring route.
	var enums, reqs int
	for _, r := range docs.Rules {
		switch {
		case r.Enum != nil:
			enums++
			if r.Component != "WidgetRequest" || r.Prop != "kind" || strings.Join(r.Enum, ",") != "round,square" {
				t.Errorf("enum rule = %+v", r)
			}
		case r.Required != nil:
			reqs++
		}
	}
	if enums != 1 || reqs != 2 {
		t.Errorf("rules: %d enums, %d required; want 1, 2", enums, reqs)
	}

	// A new section resets the running tag; a bare route under it has no prose.
	logs := docs.ByPattern["GET /api/gadgets/{id}/logs"]
	if logs.Tag != "gadgets" || logs.Description != "" || logs.Summary != "" {
		t.Errorf("bare route under a new divider = %+v", logs)
	}

	action := docs.ByPattern["POST /api/gadgets/{id}/{action}"]
	if len(action.PathParams) != 1 || action.PathParams[0].Name != "action" ||
		strings.Join(action.PathParams[0].Enum, ",") != "start,stop" ||
		action.PathParams[0].Description != "what to do to the gadget" {
		t.Errorf("path param = %+v", action.PathParams)
	}
	if len(action.QueryParams) != 1 || action.QueryParams[0].Type != "boolean" ||
		action.QueryParams[0].Description != "skip the safety check" {
		t.Errorf("query param = %+v", action.QueryParams)
	}
	if action.Deprecated == "" {
		t.Error("deprecated reason lost")
	}
}

// Broken blocks must be loud errors naming the route — a silently dropped annotation is
// documentation that lies by omission.
func TestParseRouteDocsRefusals(t *testing.T) {
	cases := map[string]string{
		"prose after directive": `
		//oapi:summary Fine
		// but this sentence comes after a directive.
		{pattern: "GET /api/x", scope: scopeNone, open: "test", h: s.h},`,

		"unknown directive": `
		//oapi:sumary typo
		{pattern: "GET /api/x", scope: scopeNone, open: "test", h: s.h},`,

		"unterminated example": `
		//oapi:example req {"never":
		{pattern: "GET /api/x", scope: scopeNone, open: "test", h: s.h},`,

		"bad enum shape": `
		//oapi:enum Widget kind a|b
		{pattern: "GET /api/x", scope: scopeNone, open: "test", h: s.h},`,

		"bad status": `
		//oapi:status 302
		{pattern: "GET /api/x", scope: scopeNone, open: "test", h: s.h},`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			src := "package api\n\nfunc (s *Server) apiRoutes() []route {\n\treturn []route{\n" +
				body + "\n\t}\n}\n"
			path := filepath.Join(t.TempDir(), "routes.go")
			if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ParseRouteDocs(path); err == nil {
				t.Fatalf("%s parsed without error", name)
			}
		})
	}
}

// The real table must parse: every route keyed, every route tagged. This is the test
// that catches a malformed annotation the moment it is written, rather than at the next
// go generate.
func TestParseRouteDocsRealTable(t *testing.T) {
	docs, err := ParseRouteDocs("server.go")
	if err != nil {
		t.Fatal(err)
	}
	metas, err := RouteMetas()
	if err != nil {
		t.Fatal(err)
	}
	if len(docs.ByPattern) != len(metas) {
		t.Fatalf("parsed %d routes, table has %d", len(docs.ByPattern), len(metas))
	}
	for _, m := range metas {
		doc, ok := docs.ByPattern[m.Pattern]
		if !ok {
			t.Errorf("%s: not found by the comment parser", m.Pattern)
			continue
		}
		if doc.Tag == "" {
			t.Errorf("%s: no section divider above it — every route lives under a ── section ──", m.Pattern)
		}
	}
}
