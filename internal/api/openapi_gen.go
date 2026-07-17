package api

// The assembly line between the route table and the generated artifacts. It lives in the
// package (not in gen.go) so the artifacts-current test can rebuild both in memory and
// byte-compare them against what is checked in — the same function the generator runs.

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Mnshahawy/daffa/internal/openapi"
)

// GenerateArtifacts builds the OpenAPI document and the api.ts client from the route
// table plus the parsed comments. Deterministic: same table, same bytes.
func GenerateArtifacts() (spec []byte, client string, err error) {
	metas, err := RouteMetas()
	if err != nil {
		return nil, "", err
	}
	docs, err := ParseRouteDocs("server.go")
	if err != nil {
		return nil, "", err
	}

	refl := openapi.NewReflector(tsNames)
	var ops []openapi.Op
	for _, m := range metas {
		doc := docs.ByPattern[m.Pattern]
		if doc == nil {
			return nil, "", fmt.Errorf("api: %s: table and comment parser disagree about existence", m.Pattern)
		}
		op := openapi.Op{
			Method:       m.Method,
			Path:         m.Path,
			Pattern:      m.Pattern,
			OperationID:  m.OperationID,
			Tag:          doc.Tag,
			Summary:      doc.Summary,
			Description:  doc.Description,
			Deprecated:   doc.Deprecated != "",
			Cap:          m.Cap,
			Scope:        m.Scope,
			SecurityNote: m.Note,
			Open:         m.Open,
			Status:       doc.Status,
			Produces:     doc.Produces,
			ReqExample:   doc.ReqExample,
			RespExample:  doc.RespExample,
			TSName:       m.TSName,
		}
		if op.Deprecated {
			op.Description = strings.TrimSpace(op.Description + "\n\nDeprecated: " + doc.Deprecated)
		}
		for _, p := range doc.PathParams {
			op.PathParams = append(op.PathParams, openapi.Param{Name: p.Name, Description: p.Description, Enum: p.Enum})
		}
		for _, p := range doc.QueryParams {
			op.QueryParams = append(op.QueryParams, openapi.Param{Name: p.Name, Type: p.Type, Description: p.Description, Enum: p.Enum})
		}

		if m.Req != nil {
			op.Req, err = refl.Add(m.Req, openapi.Request, false)
			if err != nil {
				return nil, "", fmt.Errorf("api: %s: req: %w", m.Pattern, err)
			}
		}
		if m.Resp != nil {
			op.Resp, err = refl.Add(m.Resp, openapi.Response, m.RespNullable)
			if err != nil {
				return nil, "", fmt.Errorf("api: %s: resp: %w", m.Pattern, err)
			}
			op.RespDeclared = true
		}
		if doc.Status == 204 {
			op.RespDeclared = true // 204 IS the declaration: there is no body
		}
		ops = append(ops, op)
	}

	// Property rules apply after every type is walked, so a typo in a component or
	// property name is a loud failure at generate time, not a dropped annotation.
	for _, r := range docs.Rules {
		switch {
		case r.Enum != nil:
			if err := refl.SetEnum(r.Component, r.Prop, r.Enum); err != nil {
				return nil, "", fmt.Errorf("api: %s: %w", r.Pattern, err)
			}
		case r.Required != nil:
			if err := refl.SetRequired(r.Component, r.Prop, *r.Required); err != nil {
				return nil, "", fmt.Errorf("api: %s: %w", r.Pattern, err)
			}
		}
	}

	spec, err = openapi.BuildSpec(openapi.Doc{
		Title:   "Daffa API",
		Version: "1",
		Description: "Generated from the route table in internal/api/server.go — the table IS the " +
			"authorization rule, and every operation names its capability and where it is checked. " +
			"Authenticate with a session cookie or a personal API token (see docs/tokens.md).",
	}, ops, refl.Components())
	if err != nil {
		return nil, "", err
	}

	client, err = openapi.BuildClient(ops, refl.Components())
	if err != nil {
		return nil, "", err
	}
	return spec, client, nil
}

var (
	tsMethodRe = regexp.MustCompile(`(?m)^  ([A-Za-z_$][A-Za-z0-9_$]*): \(`)
	tsTypeRe   = regexp.MustCompile(`(?m)^export (?:interface|type) ([A-Za-z_$][A-Za-z0-9_$]*)`)
)

// CheckManualDisjoint refuses generation while a name exists on both sides of the
// manual/generated boundary. The generated daffa spreads over manualDaffa, so a shared
// method name would silently shadow the handwritten one — the migration commit must
// declare the route AND delete the manual copy, together.
func CheckManualDisjoint(manual, client string) error {
	manualMethods := nameSet(tsMethodRe, manual)
	manualTypes := nameSet(tsTypeRe, manual)

	var clash []string
	for _, m := range tsMethodRe.FindAllStringSubmatch(client, -1) {
		if manualMethods[m[1]] {
			clash = append(clash, "method "+m[1])
		}
	}
	for _, m := range tsTypeRe.FindAllStringSubmatch(client, -1) {
		if manualTypes[m[1]] {
			clash = append(clash, "type "+m[1])
		}
	}
	if len(clash) > 0 {
		return fmt.Errorf("api: generated and api-manual.ts both define: %s — delete the manual copies in the same commit that declares the route",
			strings.Join(clash, ", "))
	}
	return nil
}

func nameSet(re *regexp.Regexp, text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		out[m[1]] = true
	}
	return out
}
