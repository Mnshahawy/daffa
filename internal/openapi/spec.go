package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Op is one operation, in the neutral vocabulary this package understands. internal/api
// builds these from its route table + parsed annotations; nothing here imports it back.
type Op struct {
	Method  string // GET | POST | PUT | PATCH | DELETE
	Path    string // /api/environments/{env}/logging
	Pattern string // the route table's key, verbatim (diagnostics)

	OperationID string
	Tag         string
	Summary     string
	Description string
	Deprecated  bool

	// Authorization, straight off the table. Cap+Scope XOR Open.
	Cap          string
	Scope        string
	SecurityNote string // the scope's semantics, as a sentence for the description
	Open         string // the open reason

	Status int // success status; 0 means 200. 204 ⇒ no response body.
	// Produces is a non-JSON success content type: an event stream or a file download.
	// Non-empty means no client method is generated — streams and downloads stay
	// handwritten, where their consumption model (EventSource, <a href>) actually lives.
	Produces string

	Req          *Schema // nil = no request body
	Resp         *Schema // nil = not declared (or 204/SSE)
	RespDeclared bool

	PathParams  []Param
	QueryParams []Param

	ReqExample  json.RawMessage
	RespExample json.RawMessage

	TSName string // "" = no client method is generated for this op
}

// Param documents a path or query parameter.
type Param struct {
	Name        string
	Type        string // string | integer | boolean (query only; path params are strings)
	Description string
	Enum        []string
}

// Doc is the document-level input.
type Doc struct {
	Title       string
	Version     string
	Description string
}

// ── ordered JSON ──────────────────────────────────────────────────────────────────
//
// The artifact is checked in and byte-compared by a test, so key order must be OURS —
// encoding/json sorts map keys alphabetically, which would put "delete" before "get" and
// scatter the schema fields. An insertion-ordered map keeps declaration order.

type omap struct {
	keys []string
	vals map[string]any
}

func om() *omap { return &omap{vals: map[string]any{}} }

func (m *omap) set(k string, v any) *omap {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
	return m
}

func (m *omap) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(m.vals[k])
		if err != nil {
			return nil, fmt.Errorf("openapi: marshaling %q: %w", k, err)
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// ── document assembly ─────────────────────────────────────────────────────────────

// BuildSpec renders the OpenAPI 3.1 document. Deterministic: ops in table order, tags in
// first-appearance order, components in registration order.
func BuildSpec(doc Doc, ops []Op, components []*Component) ([]byte, error) {
	root := om()
	root.set("openapi", "3.1.0")
	root.set("info", om().
		set("title", doc.Title).
		set("version", doc.Version).
		set("description", doc.Description))
	root.set("servers", []any{om().set("url", "/")})

	var tags []any
	seenTag := map[string]bool{}
	for _, op := range ops {
		if op.Tag != "" && !seenTag[op.Tag] {
			seenTag[op.Tag] = true
			tags = append(tags, om().set("name", op.Tag))
		}
	}
	root.set("tags", tags)
	root.set("security", []any{
		om().set("session", []any{}),
		om().set("token", []any{}),
	})

	paths := om()
	for i := range ops {
		op := &ops[i]
		entry, err := operation(op)
		if err != nil {
			return nil, err
		}
		item, ok := paths.vals[op.Path].(*omap)
		if !ok {
			item = om()
			paths.set(op.Path, item)
		}
		item.set(strings.ToLower(op.Method), entry)
	}
	root.set("paths", paths)

	schemas := om()
	schemas.set("ErrorBody", errorBodySchema())
	for _, c := range components {
		schemas.set(c.Name, schemaJSON(c.Schema))
	}
	root.set("components", om().
		set("securitySchemes", securitySchemes()).
		set("schemas", schemas))

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func securitySchemes() *omap {
	return om().
		set("session", om().
			set("type", "apiKey").
			set("in", "cookie").
			set("name", "__Host-daffa").
			set("description", "Browser session cookie, established by POST /api/auth/login (daffa_session when served over plain HTTP, where __Host- cannot be set). CSRF-protected; browsers only.")).
		set("token", om().
			set("type", "http").
			set("scheme", "bearer").
			set("description", "Personal API token (Authorization: Bearer daffa_…), minted under your account. Some self-service routes refuse token callers; their descriptions say so."))
}

func errorBodySchema() *omap {
	// httpx.ErrorBody: every non-2xx answer carries {code, message}, and message is
	// written for the person who reads it.
	return om().
		set("type", "object").
		set("properties", om().
			set("code", om().set("type", "string").set("description", "Stable snake_case error code.")).
			set("message", om().set("type", "string").set("description", "Human-readable sentence, written for the operator."))).
		set("required", []any{"code", "message"})
}

func operation(op *Op) (*omap, error) {
	o := om()
	if op.OperationID == "" {
		return nil, fmt.Errorf("openapi: %s has no operationId", op.Pattern)
	}
	o.set("operationId", op.OperationID)
	if op.Tag != "" {
		o.set("tags", []any{op.Tag})
	}
	if op.Summary != "" {
		o.set("summary", op.Summary)
	}
	if d := description(op); d != "" {
		o.set("description", d)
	}
	if op.Deprecated {
		o.set("deprecated", true)
	}

	// Authorization, exported. The table's justification culture carried into the spec.
	if op.Cap != "" {
		o.set("x-daffa-capability", op.Cap)
		o.set("x-daffa-scope", op.Scope)
	} else {
		o.set("x-daffa-open", op.Open)
	}

	if params := parameters(op); len(params) > 0 {
		o.set("parameters", params)
	}

	if op.Req != nil {
		content := om().set("application/json", requestContent(op))
		o.set("requestBody", om().
			set("description", "Request bodies are capped at 1 MiB; unknown fields are refused.").
			set("required", true).
			set("content", content))
	}

	o.set("responses", responses(op))
	return o, nil
}

func description(op *Op) string {
	parts := []string{}
	if op.Description != "" {
		parts = append(parts, op.Description)
	}
	if op.SecurityNote != "" {
		parts = append(parts, op.SecurityNote)
	}
	return strings.Join(parts, "\n\n")
}

func parameters(op *Op) []any {
	var out []any
	for _, p := range op.PathParams {
		s := om().set("type", "string")
		if len(p.Enum) > 0 {
			s.set("enum", strs(p.Enum))
		}
		param := om().
			set("name", p.Name).
			set("in", "path").
			set("required", true)
		if p.Description != "" {
			param.set("description", p.Description)
		}
		param.set("schema", s)
		out = append(out, param)
	}
	for _, p := range op.QueryParams {
		typ := p.Type
		if typ == "" {
			typ = "string"
		}
		s := om().set("type", typ)
		if len(p.Enum) > 0 {
			s.set("enum", strs(p.Enum))
		}
		param := om().
			set("name", p.Name).
			set("in", "query").
			set("required", false)
		if p.Description != "" {
			param.set("description", p.Description)
		}
		param.set("schema", s)
		out = append(out, param)
	}
	return out
}

func requestContent(op *Op) *omap {
	c := om().set("schema", schemaJSON(op.Req))
	if len(op.ReqExample) > 0 {
		c.set("example", op.ReqExample)
	}
	return c
}

func responses(op *Op) *omap {
	rs := om()

	status := op.Status
	if status == 0 {
		status = 200
	}
	success := om()
	switch {
	case op.Produces != "":
		if op.Produces == "text/event-stream" {
			success.set("description", "A stream of server-sent events.")
		} else {
			success.set("description", "A file download.")
		}
		content := om()
		if op.Resp != nil {
			content.set("schema", schemaJSON(op.Resp))
		}
		success.set("content", om().set(op.Produces, content))
	case status == 204 || op.Resp == nil && op.RespDeclared:
		success.set("description", "No content.")
	case op.Resp != nil:
		success.set("description", "OK.")
		content := om().set("schema", schemaJSON(op.Resp))
		if len(op.RespExample) > 0 {
			content.set("example", op.RespExample)
		}
		success.set("content", om().set("application/json", content))
	default:
		success.set("description", "Response shape not yet declared — see the coverage ratchet in internal/api.")
	}
	rs.set(fmt.Sprintf("%d", status), success)

	errRef := func(desc string) *omap {
		return om().
			set("description", desc).
			set("content", om().set("application/json",
				om().set("schema", om().set("$ref", "#/components/schemas/ErrorBody"))))
	}

	if op.Req != nil {
		rs.set("400", errRef("The request is wrong, and the message says how to fix it."))
	}
	rs.set("401", errRef("No live session or valid token."))
	switch op.Scope {
	case "global", "env", "any":
		rs.set("403", errRef("Signed in, but the capability is not held at the required scope."))
	case "stack", "job", "deployment", "monitor", "volumeSource":
		rs.set("404", errRef("Unknown id — or one the caller may not see; the two are indistinguishable on purpose."))
	}
	if op.Scope != "stack" && op.Scope != "job" && op.Scope != "deployment" &&
		op.Scope != "monitor" && op.Scope != "volumeSource" && strings.Contains(op.Path, "{") {
		rs.set("404", errRef("Nothing lives at that id."))
	}
	rs.set("500", errRef("Something went wrong on the server; the audit log has the details."))
	return rs
}

func strs(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// schemaJSON lowers a *Schema into ordered JSON.
func schemaJSON(s *Schema) any {
	switch {
	case s == nil:
		return om()
	case s.Ref != "":
		return om().set("$ref", s.Ref)
	case s.Free:
		return om()
	case len(s.OneOf) > 0:
		arms := make([]any, len(s.OneOf))
		for i, a := range s.OneOf {
			arms[i] = schemaJSON(a)
		}
		return om().set("oneOf", arms)
	}

	o := om().set("type", s.Type)
	if s.Format != "" {
		o.set("format", s.Format)
	}
	if s.ContentEncoding != "" {
		o.set("contentEncoding", s.ContentEncoding)
	}
	if s.Description != "" {
		o.set("description", s.Description)
	}
	if len(s.Enum) > 0 {
		o.set("enum", strs(s.Enum))
	}
	if s.Items != nil {
		o.set("items", schemaJSON(s.Items))
	}
	if s.AdditionalProps != nil {
		o.set("additionalProperties", schemaJSON(s.AdditionalProps))
	}
	if len(s.Properties) > 0 {
		props := om()
		for _, p := range s.Properties {
			props.set(p.Name, schemaJSON(p.Schema))
		}
		o.set("properties", props)
	}
	if len(s.Required) > 0 {
		o.set("required", strs(s.Required))
	}
	if s.Closed {
		o.set("additionalProperties", false)
	}
	return o
}
