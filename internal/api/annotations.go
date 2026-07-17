package api

// The prose half of the generator's input. RouteMetas() exports what the compiler can
// check; this file parses what it cannot — the comment blocks above each route literal
// in apiRoutes(). Plain prose becomes the operation's description (the table's existing
// comments are already documentation), and //oapi: directives add the structure prose
// cannot carry: summaries, examples, enums, statuses. Grammar reference: docs/openapi.md.

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// RouteDoc is everything the comments say about one route.
type RouteDoc struct {
	Pattern     string
	Tag         string // from the nearest preceding ── section ── divider
	Summary     string
	Description string // the prose lines, joined
	Deprecated  string // reason; "" = not deprecated

	Status      int    // //oapi:status; 0 = default 200
	Produces    string // //oapi:produces — a non-JSON success content type
	NoReq       bool   // //oapi:noreq — this POST/PUT genuinely takes no body
	ReqExample  json.RawMessage
	RespExample json.RawMessage

	PathParams  []DocParam
	QueryParams []DocParam
}

// DocParam documents one path or query parameter.
type DocParam struct {
	Name        string
	Type        string // query only
	Description string
	Enum        []string
}

// PropRule is a component-property directive: an enum or a required-ness override.
// They are collected globally (any route may declare them; convention says the owning
// create/update route does) and applied to the reflector after all types are walked.
type PropRule struct {
	Component string
	Prop      string
	Enum      []string // //oapi:enum
	Required  *bool    // //oapi:required / //oapi:optional
	Pattern   string   // where it was declared, for error messages
}

// RouteDocs is the parse result for the whole table.
type RouteDocs struct {
	ByPattern map[string]*RouteDoc
	Rules     []PropRule
}

// ParseRouteDocs reads the apiRoutes() literal in the given source file. Both `go
// generate` and `go test` run with internal/api as the working directory, so callers
// pass a bare "server.go" — the caps gen.go convention.
func ParseRouteDocs(path string) (*RouteDocs, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("api: parsing %s: %w", path, err)
	}

	table := routesLiteral(file)
	if table == nil {
		return nil, fmt.Errorf("api: %s has no apiRoutes() []route literal", path)
	}

	// Comments are matched to elements positionally: a comment group belongs to the
	// first element that starts after it. ast.NewCommentMap would do this per node, but
	// the section dividers are free-standing and must also set the running tag, so the
	// walk is explicit.
	docs := &RouteDocs{ByPattern: map[string]*RouteDoc{}}
	comments := commentsInside(fset, file, table)

	tag := ""
	ci := 0
	for _, elt := range table.Elts {
		lit, ok := elt.(*ast.CompositeLit)
		if !ok {
			continue
		}
		pattern := patternOf(lit)
		if pattern == "" {
			return nil, fmt.Errorf("api: a route literal at %s has no pattern", fset.Position(lit.Pos()))
		}

		var block []string
		for ci < len(comments) && comments[ci].End() < lit.Pos() {
			for _, c := range comments[ci].List {
				line := strings.TrimPrefix(c.Text, "//")
				if isDivider(line) {
					tag = dividerName(line)
					block = block[:0] // a divider starts a section, not a route's doc
					continue
				}
				block = append(block, line)
			}
			ci++
		}

		doc := &RouteDoc{Pattern: pattern, Tag: tag}
		if err := parseBlock(doc, docs, block); err != nil {
			return nil, fmt.Errorf("api: %s: %w", pattern, err)
		}
		docs.ByPattern[pattern] = doc
	}
	return docs, nil
}

// routesLiteral finds the []route composite literal returned by apiRoutes.
func routesLiteral(file *ast.File) *ast.CompositeLit {
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "apiRoutes" || fn.Body == nil {
			continue
		}
		for _, stmt := range fn.Body.List {
			ret, ok := stmt.(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				continue
			}
			if lit, ok := ret.Results[0].(*ast.CompositeLit); ok {
				return lit
			}
		}
	}
	return nil
}

// commentsInside returns the file's comment groups that sit inside the table literal,
// in position order.
func commentsInside(fset *token.FileSet, file *ast.File, table *ast.CompositeLit) []*ast.CommentGroup {
	var out []*ast.CommentGroup
	for _, cg := range file.Comments {
		if cg.Pos() > table.Lbrace && cg.End() < table.Rbrace {
			out = append(out, cg)
		}
	}
	return out
}

func patternOf(lit *ast.CompositeLit) string {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "pattern" {
			if s, ok := kv.Value.(*ast.BasicLit); ok && s.Kind == token.STRING {
				v, err := strconv.Unquote(s.Value)
				if err == nil {
					return v
				}
			}
		}
	}
	return ""
}

func isDivider(line string) bool {
	return strings.Contains(line, "──")
}

func dividerName(line string) string {
	return strings.TrimSpace(strings.Trim(line, " ─"))
}

// parseBlock splits a route's comment block into prose and directives. Prose must come
// first: a prose line after the first directive is refused, so a description can never
// be silently attributed to the wrong half of the block.
func parseBlock(doc *RouteDoc, docs *RouteDocs, block []string) error {
	var prose []string
	inDirectives := false

	for i := 0; i < len(block); i++ {
		line := strings.TrimSpace(block[i])
		if !strings.HasPrefix(line, "oapi:") {
			if inDirectives {
				if line == "" {
					continue
				}
				return fmt.Errorf("prose after an //oapi: directive — directives end the block, put the sentence above them: %q", line)
			}
			prose = append(prose, strings.TrimPrefix(block[i], " "))
			continue
		}
		inDirectives = true

		directive, rest, _ := strings.Cut(strings.TrimPrefix(line, "oapi:"), " ")
		rest = strings.TrimSpace(rest)
		var err error
		switch directive {
		case "summary":
			doc.Summary, err = nonEmpty(rest, "summary")
		case "example":
			i, err = parseExample(doc, block, i, rest)
		case "enum":
			err = parseEnum(docs, doc.Pattern, rest)
		case "required", "optional":
			err = parseRequired(docs, doc.Pattern, directive == "required", rest)
		case "path":
			err = parseParam(&doc.PathParams, rest, false)
		case "query":
			err = parseParam(&doc.QueryParams, rest, true)
		case "status":
			doc.Status, err = strconv.Atoi(rest)
			if err == nil && doc.Status != 200 && doc.Status != 201 && doc.Status != 202 && doc.Status != 204 {
				err = fmt.Errorf("status %d: only 200, 201, 202 and 204 are success statuses handlers write", doc.Status)
			}
		case "produces":
			// The non-JSON success bodies handlers actually write: event streams and file
			// downloads. Anything else is probably a typo for one of these.
			switch rest {
			case "text/event-stream", "application/x-pem-file", "application/octet-stream":
				doc.Produces = rest
			default:
				err = fmt.Errorf("produces %q is not a content type a handler writes", rest)
			}
		case "noreq":
			// A mutating route with no request body — a trigger, not a write. Explicit,
			// so the coverage ratchet can tell "takes nothing" from "nobody declared it".
			doc.NoReq = true
		case "deprecated":
			doc.Deprecated, err = nonEmpty(rest, "deprecated needs the reason")
		default:
			err = fmt.Errorf("unknown directive //oapi:%s", directive)
		}
		if err != nil {
			return err
		}
	}

	// Trim leading/trailing blanks, collapse the rest into paragraphs.
	doc.Description = joinProse(prose)
	return nil
}

func nonEmpty(s, what string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%s: empty", what)
	}
	return s, nil
}

// parseExample reads `req {json}` / `resp {json}`. A payload may span lines: the JSON
// accumulates across following comment lines until it validates. Reaching the end of the
// block without valid JSON is a parse error, not a silently absent example.
func parseExample(doc *RouteDoc, block []string, i int, rest string) (int, error) {
	kind, payload, _ := strings.Cut(rest, " ")
	if kind != "req" && kind != "resp" {
		return i, fmt.Errorf("example must say req or resp, got %q", kind)
	}
	buf := strings.TrimSpace(payload)
	for !json.Valid([]byte(buf)) {
		i++
		if i >= len(block) {
			return i, fmt.Errorf("example %s never became valid JSON: %s", kind, buf)
		}
		buf += strings.TrimSpace(block[i])
	}
	raw := json.RawMessage(buf)
	if kind == "req" {
		doc.ReqExample = raw
	} else {
		doc.RespExample = raw
	}
	return i, nil
}

// parseEnum reads `Component.prop a|b|c`.
func parseEnum(docs *RouteDocs, pattern, rest string) error {
	target, values, ok := strings.Cut(rest, " ")
	comp, prop, ok2 := strings.Cut(target, ".")
	if !ok || !ok2 || comp == "" || prop == "" || values == "" {
		return fmt.Errorf("enum wants `Component.prop a|b|c`, got %q", rest)
	}
	docs.Rules = append(docs.Rules, PropRule{
		Component: comp, Prop: prop, Enum: strings.Split(values, "|"), Pattern: pattern,
	})
	return nil
}

// parseRequired reads `Component.prop [Component.prop …]`.
func parseRequired(docs *RouteDocs, pattern string, required bool, rest string) error {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return fmt.Errorf("required/optional wants at least one Component.prop")
	}
	for _, f := range fields {
		comp, prop, ok := strings.Cut(f, ".")
		if !ok || comp == "" || prop == "" {
			return fmt.Errorf("required/optional wants Component.prop, got %q", f)
		}
		r := required
		docs.Rules = append(docs.Rules, PropRule{Component: comp, Prop: prop, Required: &r, Pattern: pattern})
	}
	return nil
}

// parseParam reads `name [enum=a|b|c] [type] description…` (type for query params only).
func parseParam(out *[]DocParam, rest string, query bool) error {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return fmt.Errorf("param wants `name [enum=…] description`")
	}
	p := DocParam{Name: fields[0]}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))

	if strings.HasPrefix(rest, "enum=") {
		enumStr, tail, _ := strings.Cut(strings.TrimPrefix(rest, "enum="), " ")
		p.Enum = strings.Split(enumStr, "|")
		rest = strings.TrimSpace(tail)
	}
	if query {
		typ, tail, _ := strings.Cut(rest, " ")
		switch typ {
		case "string", "integer", "boolean":
			p.Type = typ
			rest = strings.TrimSpace(tail)
		default:
			p.Type = "string" // untyped: the description starts immediately
		}
	}
	p.Description = rest
	*out = append(*out, p)
	return nil
}

// joinProse turns comment lines into paragraphs: blank comment lines split paragraphs,
// the rest joins with spaces.
func joinProse(lines []string) string {
	var paras []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			paras = append(paras, strings.Join(cur, " "))
			cur = nil
		}
	}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			flush()
			continue
		}
		cur = append(cur, l)
	}
	flush()
	return strings.Join(paras, "\n\n")
}
