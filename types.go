package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ---- types ----

// runTypes turns a JSON Schema (a bin's, or a local file) into types for
// one language, written to stdout or a file. With --check it only compares
// them with the file, for CI.
func runTypes(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("types")
	lang := fs.String("lang", "", "language: "+strings.Join(typeLangNames(), ", "))
	binID := fs.String("bin", "", "use this bin's JSON Schema: an id, or org-slug/id (needs a secret key)")
	schemaFile := fs.String("schema", "", "use a JSON Schema file instead of a bin's (no key needed)")
	name := fs.String("name", "", "name of the top-level type (default: from the bin id or the schema's title)")
	pkg := fs.String("package", "", "package (Go, Kotlin, Java), namespace (PHP, C#) or module (Ruby) for the types; Go defaults to config")
	out := fs.String("o", "", "write the types to this file instead of stdout")
	fs.StringVar(out, "out", "", "same as -o")
	check := fs.Bool("check", false, "don't write: fail if the file named by -o differs from what would be generated")
	if _, err := exactArgs(fs, args); err != nil {
		return err
	}
	l := findTypeLang(*lang)
	if *lang == "" {
		return usagef("missing --lang (one of %s)", strings.Join(typeLangNames(), ", "))
	}
	if l == nil {
		return usagef("unknown --lang %q (use one of %s)", *lang, strings.Join(typeLangNames(), ", "))
	}
	if (*binID == "") == (*schemaFile == "") {
		return usagef("give exactly one of --bin or --schema")
	}
	if *check && *out == "" {
		return usagef("--check needs -o, the file to compare")
	}
	if *out == "-" {
		*out = ""
	}

	var schema []byte
	var source string
	if *schemaFile != "" {
		b, err := os.ReadFile(*schemaFile)
		if err != nil {
			return fmt.Errorf("read %s: %w", *schemaFile, err)
		}
		schema, source = b, *schemaFile
	} else {
		c, err := a.authed(common)
		if err != nil {
			return err
		}
		var settings struct {
			HasSchema bool            `json:"has_schema"`
			Schema    json.RawMessage `json:"schema"`
		}
		if err := c.doJSON(ctx, http.MethodGet, binPath(*binID, "/settings"), nil, &settings); err != nil {
			return err
		}
		if !settings.HasSchema || len(settings.Schema) == 0 {
			return fmt.Errorf("%s has no schema, so there are no types to generate\nhint: add a JSON Schema to the bin in the dashboard, or pass a schema file with --schema", *binID)
		}
		schema, source = settings.Schema, "bin "+*binID
	}

	root := *name
	if root == "" {
		root = defaultTypeName(*binID, *schemaFile, schema)
	}
	if !isPascalIdent(root) {
		return usagef("--name %q is not a type name: use letters and digits, starting with a capital letter", root)
	}
	if l.id == "go" && *pkg == "" {
		*pkg = "config"
	}

	code, warnings, err := generateTypes(schema, l, typeOptions{name: root, pkg: *pkg, source: source})
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(a.Stderr, "warning: %s\n", w)
	}

	if *out == "" {
		_, err := io.WriteString(a.Stdout, code)
		return err
	}
	if l.id == "java" && filepath.Base(*out) != root+".java" {
		fmt.Fprintf(a.Stderr, "warning: Java needs the public type %s in a file named %s.java\n", root, root)
	}
	p := a.printer()
	if *check {
		have, err := os.ReadFile(*out)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read %s: %w", *out, err)
		}
		if err == nil && bytes.Equal(normalizeNewlines(have), []byte(code)) {
			p.ok("%s is up to date with the schema of %s", *out, source)
			return nil
		}
		what := "is out of date with"
		if err != nil {
			what = "doesn't exist yet for"
		}
		msg := fmt.Sprintf("%s %s the schema of %s", *out, what, source)
		fmt.Fprintf(a.Stderr, "error: %s\nhint: run the same command without --check, and commit the file\n", msg)
		a.annotate(annotation{file: *out, message: msg + "; run hotcfg types without --check and commit it"})
		return errReported
	}
	if err := os.WriteFile(*out, []byte(code), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	p.ok("Wrote %s types to %s", l.label, *out)
	p.field("from", source)
	p.field("type", root)
	return nil
}

// normalizeNewlines lets --check accept a file checked out with CRLF line
// endings (git on Windows).
func normalizeNewlines(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// defaultTypeName names the top-level type after the bin ("mobile-flags"
// gives MobileFlags), else the schema's title, else the schema file.
func defaultTypeName(binID, schemaFile string, schema []byte) string {
	if binID != "" {
		_, bin, ok := strings.Cut(binID, "/")
		if !ok {
			bin = binID
		}
		if n := pascalCase(identWords(bin), false); isPascalIdent(n) {
			return n
		}
	}
	var s struct {
		Title string `json:"title"`
	}
	if json.Unmarshal(schema, &s) == nil {
		if n := pascalCase(identWords(s.Title), false); isPascalIdent(n) {
			return n
		}
	}
	if schemaFile != "" {
		base := strings.TrimSuffix(filepath.Base(schemaFile), filepath.Ext(schemaFile))
		base = strings.TrimSuffix(base, ".schema")
		if n := pascalCase(identWords(base), false); isPascalIdent(n) && n != "Schema" {
			return n
		}
	}
	return "Config"
}

func isPascalIdent(s string) bool {
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}

// ---- schema → type model ----

// A tType is what the generators see of one schema: a JSON type, and for
// arrays, maps and objects what they hold. Anything the model can't say
// precisely becomes kAny (any JSON value), never a guess.
type tKind int

const (
	kAny tKind = iota
	kString
	kInt
	kNumber
	kBool
	kArray
	kMap
	kObject
	kUnion
)

type tType struct {
	kind     tKind
	nullable bool     // null is allowed too
	elem     *tType   // kArray, kMap
	obj      *tObject // kObject
	variants []*tType // kUnion: one of these (never null, never another union)
	enum     []string // kString: the allowed values, when the schema lists them
	ptr      string   // schema location, for warnings
}

type tObject struct {
	name   string
	doc    string
	fields []*tField
}

type tField struct {
	key      string
	doc      string
	typ      *tType
	required bool
}

// optional reports whether a field may be missing or null.
func (f *tField) optional() bool { return !f.required || f.typ.nullable }

type typeOptions struct {
	name   string // top-level type
	pkg    string
	source string // "bin acme/x" or a file name, for the header
}

// maxTypeDepth bounds $ref chains that never reach an object (an object is
// named, so a recursive one simply refers to itself).
const maxTypeDepth = 64

type typeModel struct {
	doc      any
	rootName string
	objects  []*tObject        // in the order found, the top-level type first
	byRef    map[string]*tType // $ref targets already built
	anchors  map[string]string // $anchor name → JSON pointer
	names    map[string]bool
	depth    int
	warnings []string
	err      error
}

func (m *typeModel) warnf(ptr, format string, args ...any) {
	m.warnings = append(m.warnings, fmt.Sprintf("%s: %s", displayPtr(ptr), fmt.Sprintf(format, args...)))
}

func displayPtr(ptr string) string {
	if ptr == "" {
		return "the schema's root"
	}
	return ptr
}

// generateTypes builds the model of schema and has the language write it.
func generateTypes(schema []byte, l *typeLang, opts typeOptions) (string, []string, error) {
	doc, err := parseOrdered(schema)
	if err != nil {
		return "", nil, fmt.Errorf("the schema is not valid JSON: %v", err)
	}
	m := &typeModel{doc: doc, rootName: opts.name, byRef: map[string]*tType{}, anchors: map[string]string{}, names: map[string]bool{opts.name: true}}
	collectAnchors(doc, "", m.anchors)
	root := m.buildRef("#", "", true)
	if m.err != nil {
		return "", nil, m.err
	}
	if root.kind != kObject {
		return "", nil, errors.New("the schema's root must be an object with \"properties\" to generate types from it")
	}
	e := &emitter{model: m, opts: opts, lang: l}
	code := l.emit(e)
	return code, append(m.warnings, e.warnings...), nil
}

// build turns one schema into a tType. hint names an object found here;
// isRoot forces the top-level name.
func (m *typeModel) build(s any, ptr, hint string, isRoot bool) *tType {
	if m.err != nil {
		return &tType{kind: kAny, ptr: ptr}
	}
	if m.depth > maxTypeDepth {
		m.warnf(ptr, "references nest too deeply; typed as any JSON value")
		return &tType{kind: kAny, ptr: ptr}
	}
	m.depth++
	defer func() { m.depth-- }()

	node, ok := s.(*oMap)
	if !ok {
		// true, false, or something that isn't a schema: any value.
		return &tType{kind: kAny, ptr: ptr}
	}
	if ref, ok := node.get("$ref").(string); ok {
		return m.buildRef(ref, ptr, isRoot)
	}
	if all, ok := node.get("allOf").([]any); ok && len(all) > 0 {
		return m.build(m.mergeAllOf(node, all, ptr), ptr, hint, isRoot)
	}
	for _, kw := range []string{"oneOf", "anyOf"} {
		if alts, ok := node.get(kw).([]any); ok && len(alts) > 0 {
			var ts []*tType
			for i, alt := range alts {
				ts = append(ts, m.build(alt, ptr+"/"+kw+"/"+strconv.Itoa(i), hint, false))
			}
			return m.union(ts, ptr)
		}
	}
	if vals, ok := node.get("enum").([]any); ok && len(vals) > 0 {
		return enumType(vals, ptr)
	}
	if node.has("const") {
		return enumType([]any{node.get("const")}, ptr)
	}

	var types []string
	switch t := node.get("type").(type) {
	case string:
		types = []string{t}
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok {
				types = append(types, s)
			}
		}
	}
	if len(types) == 0 {
		switch {
		case node.has("properties") || node.has("additionalProperties") || node.has("required"):
			types = []string{"object"}
		case node.has("items") || node.has("prefixItems"):
			types = []string{"array"}
		default:
			return &tType{kind: kAny, ptr: ptr}
		}
	}
	nullable := false
	var ts []*tType
	for _, t := range types {
		if t == "null" {
			nullable = true
			continue
		}
		ts = append(ts, m.buildTyped(node, t, ptr, hint, isRoot))
	}
	if len(ts) == 0 {
		return &tType{kind: kAny, nullable: true, ptr: ptr}
	}
	t := m.union(ts, ptr)
	if nullable {
		t.nullable = true
	}
	return t
}

func (m *typeModel) buildTyped(node *oMap, typ, ptr, hint string, isRoot bool) *tType {
	switch typ {
	case "string":
		return &tType{kind: kString, ptr: ptr}
	case "integer":
		for _, kw := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"} {
			if n, ok := node.get(kw).(json.Number); ok {
				if _, err := strconv.ParseInt(string(n), 10, 64); err != nil {
					if f, ferr := n.Float64(); ferr == nil && (f > 9.2e18 || f < -9.2e18) {
						m.warnf(ptr, "%s %s is outside the 64-bit integers most languages decode into; larger values will fail to decode", kw, n)
						break
					}
				}
			}
		}
		return &tType{kind: kInt, ptr: ptr}
	case "number":
		return &tType{kind: kNumber, ptr: ptr}
	case "boolean":
		return &tType{kind: kBool, ptr: ptr}
	case "array":
		if items := node.get("items"); items != nil {
			if _, tuple := items.([]any); !tuple {
				return &tType{kind: kArray, elem: m.build(items, ptr+"/items", singular(hint), false), ptr: ptr}
			}
		}
		if node.has("prefixItems") || node.get("items") != nil {
			m.warnf(ptr, "a tuple (items listed by position) is typed as an array of any JSON value")
		}
		return &tType{kind: kArray, elem: &tType{kind: kAny, ptr: ptr + "/items"}, ptr: ptr}
	case "object":
		props, _ := node.get("properties").(*oMap)
		if props == nil || len(props.keys) == 0 {
			if ap, ok := node.get("additionalProperties").(*oMap); ok {
				return &tType{kind: kMap, elem: m.build(ap, ptr+"/additionalProperties", hint+"Value", false), ptr: ptr}
			}
			// Keys matching patterns: a map of what the patterns allow,
			// unless other keys are allowed too.
			if pp, ok := node.get("patternProperties").(*oMap); ok && len(pp.keys) > 0 {
				if ap := node.get("additionalProperties"); ap == nil || ap == false {
					var ts []*tType
					for _, k := range pp.keys {
						ts = append(ts, m.build(pp.vals[k], ptr+"/patternProperties/"+escapePtr(k), hint+"Value", false))
					}
					return &tType{kind: kMap, elem: m.union(ts, ptr+"/patternProperties"), ptr: ptr}
				}
			}
			return &tType{kind: kMap, elem: &tType{kind: kAny, ptr: ptr}, ptr: ptr}
		}
		return m.buildObject(node, props, ptr, hint, isRoot)
	}
	m.warnf(ptr, "unknown type %q; typed as any JSON value", typ)
	return &tType{kind: kAny, ptr: ptr}
}

func (m *typeModel) buildObject(node, props *oMap, ptr, hint string, isRoot bool) *tType {
	name := m.rootName
	if !isRoot {
		if title, ok := node.get("title").(string); ok && pascalCase(identWords(title), false) != "" {
			hint = pascalCase(identWords(title), false)
		}
		name = m.uniqueTypeName(hint)
	}
	obj := &tObject{name: name}
	obj.doc, _ = node.get("description").(string)
	t := &tType{kind: kObject, obj: obj, ptr: ptr}
	m.objects = append(m.objects, obj)

	required := map[string]bool{}
	if req, ok := node.get("required").([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	for _, key := range props.keys {
		ps := props.vals[key]
		fptr := ptr + "/properties/" + escapePtr(key)
		f := &tField{key: key, required: required[key]}
		f.doc = m.description(ps)
		f.typ = m.build(ps, fptr, pascalCase(identWords(key), false), false)
		obj.fields = append(obj.fields, f)
	}
	return t
}

// description is a property's description, or that of the schema its $ref
// points to.
func (m *typeModel) description(s any) string {
	node, ok := s.(*oMap)
	if !ok {
		return ""
	}
	if d, ok := node.get("description").(string); ok {
		return d
	}
	if ref, ok := node.get("$ref").(string); ok {
		if target, ok := m.resolvePtr(ref); ok {
			if n, ok := target.(*oMap); ok {
				d, _ := n.get("description").(string)
				return d
			}
		}
	}
	return ""
}

func (m *typeModel) buildRef(ref, ptr string, isRoot bool) *tType {
	if !strings.HasPrefix(ref, "#") {
		m.err = fmt.Errorf("%s: only references inside the schema (#/$defs/...) are supported, not %q", displayPtr(ptr), ref)
		return &tType{kind: kAny, ptr: ptr}
	}
	if p, ok := m.anchors[strings.TrimPrefix(ref, "#")]; ok && !strings.HasPrefix(ref, "#/") && ref != "#" {
		ref = "#" + p // "#name" names the schema with "$anchor": "name"
	}
	if t, ok := m.byRef[ref]; ok {
		return t
	}
	target, ok := m.resolvePtr(ref)
	if !ok {
		m.err = fmt.Errorf("%s: %q points to nothing in the schema", displayPtr(ptr), ref)
		return &tType{kind: kAny, ptr: ptr}
	}
	hint := "Type"
	if i := strings.LastIndex(ref, "/"); i >= 0 && i+1 < len(ref) {
		if h := pascalCase(identWords(unescapePtr(ref[i+1:])), false); h != "" {
			hint = h
		}
	}
	// A placeholder breaks cycles: a schema that refers to itself through an
	// object gets that object's type.
	placeholder := &tType{kind: kAny, ptr: ptr}
	m.byRef[ref] = placeholder
	t := m.build(target, strings.TrimPrefix(ref, "#"), hint, isRoot)
	*placeholder = *t
	return placeholder
}

// resolvePtr follows a "#/a/b" reference into the schema.
func (m *typeModel) resolvePtr(ref string) (any, bool) {
	p := strings.TrimPrefix(ref, "#")
	cur := m.doc
	if p == "" {
		return cur, true
	}
	if !strings.HasPrefix(p, "/") {
		a, ok := m.anchors[p]
		if !ok {
			return nil, false
		}
		p = a
		if p == "" {
			return cur, true
		}
	}
	for _, tok := range strings.Split(p[1:], "/") {
		tok = unescapePtr(tok)
		switch n := cur.(type) {
		case *oMap:
			v, ok := n.vals[tok]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(n) {
				return nil, false
			}
			cur = n[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// collectAnchors records where each "$anchor" is, so "#name" can be
// resolved like "#/$defs/name".
func collectAnchors(v any, ptr string, out map[string]string) {
	switch n := v.(type) {
	case *oMap:
		if a, ok := n.get("$anchor").(string); ok {
			if _, dup := out[a]; !dup {
				out[a] = ptr
			}
		}
		for _, k := range n.keys {
			collectAnchors(n.vals[k], ptr+"/"+escapePtr(k), out)
		}
	case []any:
		for i, x := range n {
			collectAnchors(x, ptr+"/"+strconv.Itoa(i), out)
		}
	}
}

// mergeAllOf combines a schema with its allOf parts into one: their
// properties together, and every part's required keys.
func (m *typeModel) mergeAllOf(node *oMap, all []any, ptr string) *oMap {
	out := &oMap{vals: map[string]any{}}
	props := &oMap{vals: map[string]any{}}
	var required []any
	seen := map[string]bool{}
	add := func(n *oMap) {
		for _, k := range n.keys {
			switch k {
			case "allOf", "$ref":
			case "properties":
				if p, ok := n.vals[k].(*oMap); ok {
					for _, pk := range p.keys {
						props.set(pk, p.vals[pk])
					}
				}
			case "required":
				if r, ok := n.vals[k].([]any); ok {
					for _, v := range r {
						if s, ok := v.(string); ok && !seen[s] {
							seen[s] = true
							required = append(required, s)
						}
					}
				}
			default:
				if !out.has(k) {
					out.set(k, n.vals[k])
				}
			}
		}
	}
	add(node)
	for i, part := range all {
		n, ok := part.(*oMap)
		for depth := 0; ok && n.has("$ref") && depth < maxTypeDepth; depth++ {
			ref, _ := n.get("$ref").(string)
			target, found := m.resolvePtr(ref)
			if !found {
				m.warnf(ptr+"/allOf/"+strconv.Itoa(i), "%q points to nothing in the schema; left out", ref)
				ok = false
				break
			}
			add(n)
			n, ok = target.(*oMap)
		}
		if ok {
			if sub, has := n.get("allOf").([]any); has {
				n = m.mergeAllOf(n, sub, ptr+"/allOf/"+strconv.Itoa(i))
			}
			add(n)
		}
	}
	if len(props.keys) > 0 {
		out.set("properties", props)
	}
	if len(required) > 0 {
		out.set("required", required)
	}
	return out
}

// union combines alternatives: null makes the rest nullable, integer and
// number make a number, equal alternatives collapse, anything else is a
// kUnion for the languages that have one.
func (m *typeModel) union(ts []*tType, ptr string) *tType {
	nullable := false
	var flat []*tType
	for _, t := range ts {
		if t.nullable {
			nullable = true
		}
		if t.kind == kUnion {
			flat = append(flat, t.variants...)
			continue
		}
		if t.kind == kAny && t.nullable && t.elem == nil && t.obj == nil {
			continue // "null" alone
		}
		flat = append(flat, t)
	}
	var out []*tType
	for _, t := range flat {
		if t.kind == kAny {
			return &tType{kind: kAny, nullable: nullable, ptr: ptr}
		}
		merged := false
		for i, o := range out {
			switch {
			case sameType(o, t):
				if o.kind == kString {
					out[i] = mergeEnums(o, t)
				}
				merged = true
			case (o.kind == kInt && t.kind == kNumber) || (o.kind == kNumber && t.kind == kInt):
				out[i] = &tType{kind: kNumber, ptr: o.ptr}
				merged = true
			}
			if merged {
				break
			}
		}
		if !merged {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return &tType{kind: kAny, nullable: true, ptr: ptr}
	}
	if len(out) == 1 {
		t := *out[0]
		t.nullable = t.nullable || nullable
		return &t
	}
	for i, t := range out {
		c := *t // a copy: t may be a $ref's type, shared with other uses
		c.nullable = false
		out[i] = &c
	}
	return &tType{kind: kUnion, variants: out, nullable: nullable, ptr: ptr}
}

func sameType(a, b *tType) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case kObject:
		return a.obj == b.obj
	case kArray, kMap:
		return sameType(a.elem, b.elem) && a.elem.nullable == b.elem.nullable
	}
	return true
}

// mergeEnums: "a" | "b" with "c" is "a" | "b" | "c"; with any string, any string.
func mergeEnums(a, b *tType) *tType {
	if a.enum == nil || b.enum == nil {
		return &tType{kind: kString, ptr: a.ptr}
	}
	vals := append(append([]string{}, a.enum...), b.enum...)
	return &tType{kind: kString, enum: dedupe(vals), ptr: a.ptr}
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// enumType types the values an enum or const allows.
func enumType(vals []any, ptr string) *tType {
	nullable := false
	kinds := map[tKind]bool{}
	var strs []string
	for _, v := range vals {
		switch x := v.(type) {
		case nil:
			nullable = true
		case string:
			kinds[kString] = true
			strs = append(strs, x)
		case bool:
			kinds[kBool] = true
		case json.Number:
			if strings.ContainsAny(string(x), ".eE") {
				kinds[kNumber] = true
			} else {
				kinds[kInt] = true
			}
		default:
			kinds[kAny] = true
		}
	}
	if kinds[kInt] && kinds[kNumber] {
		delete(kinds, kInt)
	}
	if len(kinds) != 1 {
		return &tType{kind: kAny, nullable: nullable || len(kinds) == 0, ptr: ptr}
	}
	for k := range kinds {
		t := &tType{kind: k, nullable: nullable, ptr: ptr}
		if k == kString {
			t.enum = dedupe(strs)
		}
		return t
	}
	panic("unreachable")
}

// reservedTypeNames are names an object must not take, in any language: the
// languages' own types and the generated helpers. A clash is prefixed with
// the top-level type's name (Data becomes AppConfigData).
var reservedTypeNames = setOf(
	"Any", "Array", "Bool", "Boolean", "Byte", "Callable", "Char", "Class", "Collections",
	"Color", "Comparable", "Data", "Date", "Decimal", "Dictionary", "Double", "Enum", "Error",
	"Exception", "Float", "Function", "Hash", "Int", "Integer", "Iterable", "Iterator", "JSON",
	"JSONArray", "JSONObject", "JSONValue", "Kernel", "List", "Literal", "Long", "Map", "Math",
	"Module", "Node", "Nothing", "NotRequired", "Number", "Object", "Optional", "Promise",
	"Range", "Record", "RefCounted", "Resource", "Result", "Self", "Sendable", "Set",
	"Short", "Signal", "String", "Struct", "Symbol", "Type", "TypedDict", "URL", "Unit",
	"Variant", "Vector2", "Vector3", "Void",
	// What the generated Java uses (imports, @SuppressWarnings); PHP's own.
	"ArrayList", "LinkedHashMap", "Mixed", "Never", "Null", "Parent", "Static", "SuppressWarnings",
)

// article is "a" or "an" for a name, by its first letter.
func article(name string) string {
	if name != "" && strings.ContainsRune("AEIOUaeiou", rune(name[0])) {
		return "an"
	}
	return "a"
}

func (m *typeModel) uniqueTypeName(hint string) string {
	if hint == "" {
		hint = "Item"
	}
	if hint[0] >= '0' && hint[0] <= '9' {
		hint = "T" + hint
	}
	if reservedTypeNames[hint] {
		hint = m.rootName + hint
	}
	name := hint
	for i := 2; m.names[name]; i++ {
		name = hint + strconv.Itoa(i)
	}
	m.names[name] = true
	return name
}

// singular names an array's element type: Overrides holds Override.
func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 4:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "shes"), strings.HasSuffix(s, "ches"), strings.HasSuffix(s, "xes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "ss"), strings.HasSuffix(s, "us"), strings.HasSuffix(s, "is"):
		return s + "Item"
	case strings.HasSuffix(s, "s") && len(s) > 3:
		return s[:len(s)-1]
	}
	return s + "Item"
}

func escapePtr(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

func unescapePtr(s string) string {
	return strings.NewReplacer("~1", "/", "~0", "~").Replace(s)
}

// ---- identifiers ----

// identWords splits a JSON key into words: "max_upload_mb", "maxUploadMB"
// and "max-upload-MB" all give max, upload, mb (in their own case). Only
// ASCII letters and digits are kept.
func identWords(s string) []string {
	var words []string
	var cur []rune
	rs := []rune(s)
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = nil
		}
	}
	isUpper := func(r rune) bool { return r >= 'A' && r <= 'Z' }
	isLower := func(r rune) bool { return r >= 'a' && r <= 'z' }
	isDigit := func(r rune) bool { return r >= '0' && r <= '9' }
	for i, r := range rs {
		if !isUpper(r) && !isLower(r) && !isDigit(r) {
			flush()
			continue
		}
		if len(cur) > 0 {
			prev := cur[len(cur)-1]
			switch {
			case isUpper(r) && (isLower(prev) || isDigit(prev)):
				flush()
			case isUpper(r) && isUpper(prev) && i+1 < len(rs) && isLower(rs[i+1]):
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return words
}

// goInitialisms are written in capitals in Go names (UserID, not UserId).
var goInitialisms = setOf("api", "cpu", "css", "dns", "eta", "html", "http", "https", "id", "ip", "json", "os", "sdk", "sql", "ttl", "ui", "uri", "url", "utc", "uuid", "xml")

func pascalCase(words []string, initialisms bool) string {
	var b strings.Builder
	for _, w := range words {
		lw := strings.ToLower(w)
		if initialisms && goInitialisms[lw] {
			b.WriteString(strings.ToUpper(lw))
			continue
		}
		b.WriteString(strings.ToUpper(lw[:1]) + lw[1:])
	}
	return b.String()
}

func camelCase(words []string) string {
	if len(words) == 0 {
		return ""
	}
	return strings.ToLower(words[0]) + pascalCase(words[1:], false)
}

func snakeCase(words []string) string {
	ws := make([]string, len(words))
	for i, w := range words {
		ws[i] = strings.ToLower(w)
	}
	return strings.Join(ws, "_")
}

// fieldNamer gives an object's fields distinct names in one style.
type fieldNamer struct {
	style    func([]string) string
	reserved map[string]bool
	escape   func(string) string // a reserved word made usable
	used     map[string]bool
}

func (n *fieldNamer) name(key string) string {
	s := n.style(identWords(key))
	if s == "" {
		s = n.style([]string{"field"})
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = n.style([]string{"v"}) + s
	}
	if n.reserved[s] {
		s = n.escape(s)
	}
	base := s
	for i := 2; n.used[s]; i++ {
		s = base + strconv.Itoa(i)
	}
	if n.used == nil {
		n.used = map[string]bool{}
	}
	n.used[s] = true
	return s
}

func setOf(ss ...string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// ---- ordered JSON ----

// oMap is a JSON object that keeps its keys in order, so the types list
// fields in the order the schema does.
type oMap struct {
	keys []string
	vals map[string]any
}

func (o *oMap) get(k string) any  { return o.vals[k] }
func (o *oMap) has(k string) bool { _, ok := o.vals[k]; return ok }
func (o *oMap) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// parseOrdered decodes one JSON value with objects as *oMap and numbers as
// json.Number.
func parseOrdered(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := decodeOrdered(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return v, nil
}

func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &oMap{vals: map[string]any{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				o.set(k, v)
			}
			_, err := dec.Token()
			return o, err
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := dec.Token()
			return arr, err
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return t, nil
	}
}

// ---- languages ----

type typeLang struct {
	id      string
	aliases []string
	label   string
	emit    func(*emitter) string
}

// typeLangs is filled in init, as the emitters refer back to it.
var typeLangs []*typeLang

func init() {
	typeLangs = []*typeLang{
		{id: "typescript", aliases: []string{"ts"}, label: "TypeScript", emit: emitTypeScript},
		{id: "swift", label: "Swift", emit: emitSwift},
		{id: "kotlin", aliases: []string{"kt"}, label: "Kotlin", emit: emitKotlin},
		{id: "java", label: "Java", emit: emitJava},
		{id: "python", aliases: []string{"py"}, label: "Python", emit: emitPython},
		{id: "php", label: "PHP", emit: emitPHP},
		{id: "ruby", aliases: []string{"rb"}, label: "Ruby", emit: emitRuby},
		{id: "go", label: "Go", emit: emitGo},
		{id: "gdscript", aliases: []string{"gd", "godot"}, label: "GDScript", emit: emitGDScript},
		{id: "csharp", aliases: []string{"cs", "c#", "unity"}, label: "C#", emit: emitCSharp},
	}
}

func typeLangNames() []string {
	var names []string
	for _, l := range typeLangs {
		names = append(names, l.id)
	}
	return names
}

func findTypeLang(s string) *typeLang {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, l := range typeLangs {
		if l.id == s {
			return l
		}
		for _, a := range l.aliases {
			if a == s {
				return l
			}
		}
	}
	return nil
}

// emitter is shared state while one language writes the model.
type emitter struct {
	model    *typeModel
	opts     typeOptions
	lang     *typeLang
	b        strings.Builder
	warnings []string
	warned   map[string]bool
}

func (e *emitter) line(format string, args ...any) {
	if format == "" {
		e.b.WriteString("\n")
		return
	}
	fmt.Fprintf(&e.b, format, args...)
	e.b.WriteString("\n")
}

// degrade notes once per location that a language can't express a type.
func (e *emitter) degrade(t *tType, what string) {
	key := t.ptr + "\x00" + what
	if e.warned == nil {
		e.warned = map[string]bool{}
	}
	if e.warned[key] {
		return
	}
	e.warned[key] = true
	e.warnings = append(e.warnings, fmt.Sprintf("%s: %s", displayPtr(t.ptr), what))
}

// header is the generated-file notice, one comment line per element.
func (e *emitter) header() []string {
	return []string{
		"Generated by hotcfg types from the JSON Schema of " + e.opts.source + ".",
		"Do not edit: run hotcfg types again when the schema changes.",
	}
}

// objects returns the objects with the top-level type first.
func (e *emitter) objects() []*tObject { return e.model.objects }

// objectsLeavesFirst orders objects so each comes after those it uses (for
// languages that need a name defined before it is used).
func (e *emitter) objectsLeavesFirst() []*tObject {
	var out []*tObject
	seen := map[*tObject]bool{}
	var visit func(o *tObject)
	var visitType func(t *tType)
	visitType = func(t *tType) {
		switch t.kind {
		case kObject:
			visit(t.obj)
		case kArray, kMap:
			visitType(t.elem)
		case kUnion:
			for _, v := range t.variants {
				visitType(v)
			}
		}
	}
	visit = func(o *tObject) {
		if seen[o] {
			return
		}
		seen[o] = true
		for _, f := range o.fields {
			visitType(f.typ)
		}
		out = append(out, o)
	}
	for _, o := range e.model.objects {
		visit(o)
	}
	return out
}

// docLines splits a description into lines, trimmed, without blank ends.
// Every line break a language may read (\r, U+0085, U+2028 and U+2029 as
// well as \n) splits a line, and other control characters become spaces:
// a description goes into line comments, so text after a break the
// generator didn't see would be code.
func docLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == 0x85 || r == 0x2028 || r == 0x2029:
			return '\n'
		case r == '\t':
			return r
		case r < 0x20 || r == 0x7f:
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return lines
}

// enumNote is "One of: a, b." for languages that type an enum as a string.
func enumNote(t *tType) string {
	if t.kind != kString || len(t.enum) == 0 {
		return ""
	}
	q := make([]string, len(t.enum))
	for i, v := range t.enum {
		q[i] = strconv.Quote(v)
	}
	if len(q) == 1 {
		return "Always " + q[0] + "."
	}
	return "One of " + strings.Join(q, ", ") + "."
}

// fieldDoc is a field's description plus its enum values.
func fieldDoc(f *tField) []string {
	lines := docLines(f.doc)
	t := f.typ
	for t.kind == kArray {
		t = t.elem
	}
	if n := enumNote(t); n != "" {
		lines = append(lines, n)
	}
	return lines
}

// sortedKeys is for deterministic output from maps.
func sortedKeys(m map[string]bool) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
