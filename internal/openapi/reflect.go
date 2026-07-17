// Package openapi turns Go types and route metadata into an OpenAPI 3.1 document and a
// TypeScript client. It is the machinery behind `go generate ./internal/api`; see
// docs/openapi.md.
//
// The package is deliberately stdlib-only and knows nothing about internal/api: it
// consumes reflect.Types and neutral Op structs. One reflector walk feeds BOTH emitters
// (JSON Schema in spec.go, TypeScript in tsemit.go), which is what keeps the spec and the
// client from drifting apart — they cannot disagree about a shape they share.
package openapi

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Schema is the neutral shape node both emitters read. Exactly one "kind" is set: Ref,
// OneOf, Free, or Type (with its accompaniments).
type Schema struct {
	Ref  string // "#/components/schemas/X" — the component's name is the last segment
	Free bool   // the true schema {}: anything goes (Go `any`)

	Type            string // string | integer | number | boolean | object | array | null
	Format          string // date-time on time.Time — the repo's RFC3339 convention
	ContentEncoding string // base64 on []byte
	Items           *Schema
	AdditionalProps *Schema // maps: map[string]T
	Properties      []Property
	Required        []string
	Closed          bool // request components: httpx.Decode uses DisallowUnknownFields
	Enum            []string
	OneOf           []*Schema
	Description     string
}

// Property keeps declaration order — the emitted artifacts are checked in and
// byte-compared, so iteration order must be the struct's, never a map's. Whether a
// property may be ABSENT is the parent schema's Required list, not a field here: the
// list is mode-dependent and //oapi:required edits it after the walk.
type Property struct {
	Name   string
	Schema *Schema
}

// IsRequired reports whether a property must be present — the emitters' one source of
// truth for both JSON Schema `required` and the TS `?` marker.
func (s *Schema) IsRequired(prop string) bool {
	for _, r := range s.Required {
		if r == prop {
			return true
		}
	}
	return false
}

// Mode picks the required-ness rules. They are asymmetric on purpose: encoding/json
// always emits a response field unless omitempty hides it, while httpx.Decode treats an
// absent request field as its zero value — so response fields default to required and
// request fields default to optional (with //oapi:required as the opt-in).
type Mode int

const (
	Response Mode = iota
	Request
)

// Component is one named schema, in registration order.
type Component struct {
	Name   string
	Mode   Mode
	Schema *Schema // always the expanded object schema, never a Ref
}

// Reflector accumulates components across routes. One instance per generation run.
type Reflector struct {
	names      map[reflect.Type]string // overrides + assignments, keyed by struct type
	components []*Component
	byName     map[string]*compEntry
}

type compEntry struct {
	comp *Component
	typ  reflect.Type
	mode Mode
}

// NewReflector takes the name-override table (Go type → component/interface name) for
// the cases where the wire name is load-bearing in the views (stackView → Stack).
func NewReflector(overrides map[reflect.Type]string) *Reflector {
	names := map[reflect.Type]string{}
	for t, n := range overrides {
		names[t] = n
	}
	return &Reflector{names: names, byName: map[string]*compEntry{}}
}

var timeType = reflect.TypeOf(time.Time{})

// Add walks a top-level payload type and returns the schema to reference it by. Named
// structs become components; the returned schema is a Ref (possibly wrapped in array or
// nullability). nullable applies at the top level only — a handler that answers `null`
// when the thing is unset.
func (r *Reflector) Add(t reflect.Type, mode Mode, nullable bool) (*Schema, error) {
	s, err := r.walk(t, mode)
	if err != nil {
		return nil, err
	}
	if nullable {
		return &Schema{OneOf: []*Schema{s, {Type: "null"}}}, nil
	}
	return s, nil
}

func (r *Reflector) walk(t reflect.Type, mode Mode) (*Schema, error) {
	switch t.Kind() {
	case reflect.Pointer:
		inner, err := r.walk(t.Elem(), mode)
		if err != nil {
			return nil, err
		}
		return inner, nil // nullability is decided by the FIELD (or Add), not here

	case reflect.String:
		return &Schema{Type: "string"}, nil
	case reflect.Bool:
		return &Schema{Type: "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}, nil

	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			// []byte marshals as base64 text; documenting an array of integers would
			// describe bytes that never appear on the wire.
			return &Schema{Type: "string", ContentEncoding: "base64"}, nil
		}
		items, err := r.walk(t.Elem(), mode)
		if err != nil {
			return nil, err
		}
		return &Schema{Type: "array", Items: items}, nil

	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("openapi: %s: only string-keyed maps exist on a JSON wire", t)
		}
		if t.Elem().Kind() == reflect.Interface {
			// map[string]any: an object with no stated shape. Representable, but the
			// ratchet prefers these stay undeclared until lifted to a named struct.
			return &Schema{Type: "object"}, nil
		}
		val, err := r.walk(t.Elem(), mode)
		if err != nil {
			return nil, err
		}
		return &Schema{Type: "object", AdditionalProps: val}, nil

	case reflect.Interface:
		return &Schema{Free: true}, nil

	case reflect.Struct:
		if t == timeType {
			return &Schema{Type: "string", Format: "date-time"}, nil
		}
		if t.Name() == "" {
			return r.object(t, mode) // anonymous struct: inline, no component
		}
		name, err := r.register(t, mode)
		if err != nil {
			return nil, err
		}
		return &Schema{Ref: "#/components/schemas/" + name}, nil

	default:
		return nil, fmt.Errorf("openapi: %s (%s) has no JSON wire shape", t, t.Kind())
	}
}

// register names a struct type and builds its component once. Cycles terminate here: the
// name is claimed BEFORE the walk, so a self-referential type resolves to its own Ref.
func (r *Reflector) register(t reflect.Type, mode Mode) (string, error) {
	name := r.componentName(t, mode)
	if e, ok := r.byName[name]; ok {
		if e.typ != t || e.mode != mode {
			return "", fmt.Errorf("openapi: %s and %s both want the component name %q — add a tsNames override for one of them", e.typ, t, name)
		}
		return name, nil
	}
	comp := &Component{Name: name, Mode: mode}
	r.byName[name] = &compEntry{comp: comp, typ: t, mode: mode}
	r.components = append(r.components, comp)

	obj, err := r.object(t, mode)
	if err != nil {
		return "", err
	}
	if mode == Request {
		obj.Closed = true // httpx.Decode refuses unknown fields
	}
	comp.Schema = obj
	return name, nil
}

// componentName is the bare Go type name, exported-cased — then the override table.
// Request-mode components always carry the Request suffix (Go's own xxxRequest types
// already do), so a dual-use type like store.Monitor yields two components — Monitor and
// MonitorRequest — whose required-ness rules differ. The naming is a pure function of
// (type, mode): registration order can never change what something is called.
func (r *Reflector) componentName(t reflect.Type, mode Mode) string {
	name, ok := r.names[t]
	if !ok {
		name = strings.ToUpper(t.Name()[:1]) + t.Name()[1:]
	}
	if mode == Request && !strings.HasSuffix(name, "Request") {
		name += "Request"
	}
	return name
}

// object builds the property list for a struct type, honouring encoding/json semantics
// via reflect.VisibleFields: embedded promotion, shadowing, json:"-", tag renames.
func (r *Reflector) object(t reflect.Type, mode Mode) (*Schema, error) {
	obj := &Schema{Type: "object"}
	for _, f := range reflect.VisibleFields(t) {
		if f.Anonymous || !f.IsExported() {
			continue // VisibleFields lists the embedded struct itself too; its fields follow
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		hasOpt := func(o string) bool {
			for _, p := range strings.Split(opts, ",") {
				if p == o {
					return true
				}
			}
			return false
		}

		var fs *Schema
		var err error
		if hasOpt("string") {
			fs = &Schema{Type: "string"} // json:",string": the wire IS a string
		} else {
			fs, err = r.walk(f.Type, mode)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", t, f.Name, err)
			}
		}

		// encoding/json's exact behaviour, mirrored: a nil pointer under omitempty is
		// OMITTED (the field is optional, never null); without omitempty it is emitted
		// as NULL (the field is always present, nullable). Conflating the two produced
		// `deployed_at?: string | null` for a field that can never be null — and every
		// view that handled only undefined stopped typechecking.
		ptr := f.Type.Kind() == reflect.Pointer
		omit := hasOpt("omitempty")
		if ptr && !omit {
			fs = &Schema{OneOf: []*Schema{fs, {Type: "null"}}}
		}

		obj.Properties = append(obj.Properties, Property{Name: name, Schema: fs})
		// Response fields are required unless omitempty can hide them; request fields
		// are never required by default — httpx.Decode reads absent as zero, and the
		// handler's own validation decides what it refuses. //oapi:required opts in.
		if mode == Response && !omit {
			obj.Required = append(obj.Required, name)
		}
	}
	return obj, nil
}

// Components returns every registered component in registration order.
func (r *Reflector) Components() []*Component { return r.components }

// SetEnum pins an enum onto a component property — the //oapi:enum directive. It is
// applied after the walk, so a typo in the component or property name is a loud error
// rather than a silently ignored annotation.
func (r *Reflector) SetEnum(component, prop string, values []string) error {
	p, err := r.property(component, prop, "enum")
	if err != nil {
		return err
	}
	target := p.Schema
	if len(target.OneOf) > 0 { // nullable field: the enum belongs to the non-null arm
		target = target.OneOf[0]
	}
	if target.Type == "array" && target.Items != nil {
		target = target.Items // []string: the enum constrains the elements
	}
	if target.Type != "string" {
		return fmt.Errorf("openapi: enum on %s.%s: only string properties (or string arrays) carry enums", component, prop)
	}
	if len(target.Enum) > 0 && !equalStrings(target.Enum, values) {
		return fmt.Errorf("openapi: conflicting enums declared for %s.%s", component, prop)
	}
	target.Enum = values
	return nil
}

// SetRequired overrides a property's required-ness — //oapi:required and //oapi:optional.
func (r *Reflector) SetRequired(component, prop string, required bool) error {
	e, ok := r.byName[component]
	if !ok {
		return fmt.Errorf("openapi: required override names unknown component %q", component)
	}
	if _, err := r.property(component, prop, "required override"); err != nil {
		return err
	}
	s := e.comp.Schema
	idx := -1
	for i, req := range s.Required {
		if req == prop {
			idx = i
		}
	}
	switch {
	case required && idx < 0:
		s.Required = append(s.Required, prop)
	case !required && idx >= 0:
		s.Required = append(s.Required[:idx], s.Required[idx+1:]...)
	}
	// Keep the list in property order, not directive order — determinism again.
	order := map[string]int{}
	for i, p := range s.Properties {
		order[p.Name] = i
	}
	sort.SliceStable(s.Required, func(i, j int) bool { return order[s.Required[i]] < order[s.Required[j]] })
	return nil
}

func (r *Reflector) property(component, prop, what string) (*Property, error) {
	e, ok := r.byName[component]
	if !ok {
		return nil, fmt.Errorf("openapi: %s names unknown component %q", what, component)
	}
	for i := range e.comp.Schema.Properties {
		if e.comp.Schema.Properties[i].Name == prop {
			return &e.comp.Schema.Properties[i], nil
		}
	}
	return nil, fmt.Errorf("openapi: %s names unknown property %q on %q", what, prop, component)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
