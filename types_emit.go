package cli

import (
	"fmt"
	"go/format"
	"strconv"
	"strings"
	"unicode/utf8"
)

// One emitter per language. Each writes every object of the model as that
// language's usual record type, in a single file, with no dependencies
// beyond what the docs' fetch recipe for that language already uses.

// quoteC quotes s with C-style escapes (", \, control characters as
// \uXXXX), the form TypeScript, Java, Kotlin, Python and GDScript share.
// Other text is kept as is: every generated file is UTF-8.
func quoteC(s string, extra map[rune]string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if e, ok := extra[r]; ok {
			b.WriteString(e)
			continue
		}
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			// U+0085, U+2028 and U+2029 end a line in C#, so they can't
			// appear in a string as they are.
			if r < 0x20 || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029 || r == utf8.RuneError {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func quoteJS(s string) string { return quoteC(s, nil) }

var kotlinExtra = map[rune]string{'$': `\$`}

func quoteKotlin(s string) string { return quoteC(s, kotlinExtra) }

// quoteSwift uses \u{X}, Swift's only form of escape by code point.
func quoteSwift(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u{%x}`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// quoteSingle is a single-quoted PHP or Ruby string: only \ and ' are
// special.
func quoteSingle(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func appendUnderscore(s string) string { return s + "_" }

// ---- TypeScript ----

func emitTypeScript(e *emitter) string {
	for _, h := range e.header() {
		e.line("// %s", h)
	}
	for _, o := range e.objects() {
		e.line("")
		tsDoc(e, "", docLines(o.doc))
		e.line("export interface %s {", o.name)
		for _, f := range o.fields {
			tsDoc(e, "  ", docLines(f.doc))
			key := f.key
			if !isJSIdent(key) {
				key = quoteJS(key)
			}
			opt := ""
			if !f.required {
				opt = "?"
			}
			e.line("  %s%s: %s;", key, opt, tsType(f.typ))
		}
		e.line("}")
	}
	return e.b.String()
}

func tsDoc(e *emitter, indent string, lines []string) {
	if len(lines) == 0 {
		return
	}
	for i, l := range lines {
		lines[i] = strings.ReplaceAll(l, "*/", `*\/`)
	}
	if len(lines) == 1 {
		e.line("%s/** %s */", indent, lines[0])
		return
	}
	e.line("%s/**", indent)
	for _, l := range lines {
		e.line("%s", strings.TrimRight(indent+" * "+l, " "))
	}
	e.line("%s */", indent)
}

func isJSIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func tsType(t *tType) string {
	var s string
	switch t.kind {
	case kString:
		if len(t.enum) > 0 {
			q := make([]string, len(t.enum))
			for i, v := range t.enum {
				q[i] = quoteJS(v)
			}
			s = strings.Join(q, " | ")
		} else {
			s = "string"
		}
	case kInt, kNumber:
		s = "number"
	case kBool:
		s = "boolean"
	case kArray:
		inner := tsType(t.elem)
		if strings.Contains(inner, " | ") {
			inner = "(" + inner + ")"
		}
		s = inner + "[]"
	case kMap:
		s = "Record<string, " + tsType(t.elem) + ">"
	case kObject:
		s = t.obj.name
	case kUnion:
		vs := make([]string, len(t.variants))
		for i, v := range t.variants {
			vs[i] = tsType(v)
		}
		s = strings.Join(vs, " | ")
	default:
		s = "unknown"
	}
	if t.nullable && t.kind != kAny {
		s += " | null"
	}
	return s
}

// ---- Swift ----

var swiftKeywords = setOf("Any", "Protocol", "Self", "Type", "as", "associatedtype", "break", "case", "catch",
	"class", "continue", "default", "defer", "deinit", "do", "else", "enum", "extension", "fallthrough",
	"false", "fileprivate", "for", "func", "guard", "if", "import", "in", "init", "inout", "internal", "is",
	"let", "nil", "open", "operator", "private", "precedencegroup", "protocol", "public", "repeat",
	"rethrows", "return", "self", "static", "struct", "subscript", "super", "switch", "throw", "throws",
	"true", "try", "typealias", "var", "where", "while")

func emitSwift(e *emitter) string {
	for _, h := range e.header() {
		e.line("// %s", h)
	}
	usesJSON := false
	for _, o := range e.objects() {
		e.line("")
		for _, l := range docLines(o.doc) {
			e.line("/// %s", l)
		}
		if swiftCyclic(o) {
			// A struct can't hold itself, even optionally; a class can.
			e.line("final class %s: Codable, Sendable {", o.name)
		} else {
			e.line("struct %s: Codable, Sendable {", o.name)
		}
		n := fieldNamer{style: camelCase, reserved: swiftKeywords, escape: swiftEscape}
		type ck struct{ name, key string }
		var keys []ck
		rename := false
		for _, f := range o.fields {
			for _, l := range fieldDoc(f) {
				e.line("    /// %s", l)
			}
			name := n.name(f.key)
			t := swiftType(e, f.typ, &usesJSON)
			if f.optional() && !strings.HasSuffix(t, "?") {
				t += "?"
			}
			e.line("    let %s: %s", name, t)
			keys = append(keys, ck{name, f.key})
			if strings.Trim(name, "`") != f.key {
				rename = true
			}
		}
		if rename {
			e.line("")
			e.line("    enum CodingKeys: String, CodingKey {")
			for _, k := range keys {
				if strings.Trim(k.name, "`") == k.key {
					e.line("        case %s", k.name)
				} else {
					e.line("        case %s = %s", k.name, quoteSwift(k.key))
				}
			}
			e.line("    }")
		}
		e.line("}")
	}
	if usesJSON {
		e.line("")
		e.b.WriteString(swiftJSONValue)
	}
	return e.b.String()
}

// swiftCyclic reports whether o contains itself through object fields
// alone. Arrays and dictionaries store their elements indirectly, so a
// cycle through them is fine for a struct.
func swiftCyclic(o *tObject) bool {
	seen := map[*tObject]bool{}
	var reaches func(cur *tObject) bool
	reaches = func(cur *tObject) bool {
		for _, f := range cur.fields {
			if f.typ.kind != kObject {
				continue
			}
			next := f.typ.obj
			if next == o {
				return true
			}
			if !seen[next] {
				seen[next] = true
				if reaches(next) {
					return true
				}
			}
		}
		return false
	}
	return reaches(o)
}

func swiftType(e *emitter, t *tType, usesJSON *bool) string {
	var s string
	switch t.kind {
	case kString:
		s = "String"
	case kInt:
		s = "Int"
	case kNumber:
		s = "Double"
	case kBool:
		s = "Bool"
	case kArray:
		s = "[" + swiftType(e, t.elem, usesJSON) + "]"
	case kMap:
		s = "[String: " + swiftType(e, t.elem, usesJSON) + "]"
	case kObject:
		s = t.obj.name
	case kUnion:
		e.degrade(t, "Swift has no type for one of several types; typed as JSONValue")
		fallthrough
	default:
		*usesJSON = true
		s = "JSONValue"
	}
	if t.nullable {
		s += "?"
	}
	return s
}

// swiftEscape makes a keyword usable as a property name. Backticks work for
// most, but "x.self" and "x.init" mean something else after a dot, so those
// two get a suffix instead.
func swiftEscape(s string) string {
	if s == "self" || s == "init" {
		return s + "Value"
	}
	return "`" + s + "`"
}

const swiftJSONValue = `/// Any JSON value, for the parts of the schema that have no single type.
enum JSONValue: Codable, Sendable, Equatable {
    case string(String)
    case number(Double)
    case bool(Bool)
    case array([JSONValue])
    case object([String: JSONValue])
    case null

    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() {
            self = .null
        } else if let v = try? c.decode(Bool.self) {
            self = .bool(v)
        } else if let v = try? c.decode(Double.self) {
            self = .number(v)
        } else if let v = try? c.decode(String.self) {
            self = .string(v)
        } else if let v = try? c.decode([JSONValue].self) {
            self = .array(v)
        } else {
            self = .object(try c.decode([String: JSONValue].self))
        }
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .string(let v): try c.encode(v)
        case .number(let v): try c.encode(v)
        case .bool(let v): try c.encode(v)
        case .array(let v): try c.encode(v)
        case .object(let v): try c.encode(v)
        case .null: try c.encodeNil()
        }
    }
}
`

// ---- Kotlin (org.json, built into Android) ----

var kotlinKeywords = setOf("as", "break", "class", "continue", "do", "else", "false", "for", "fun", "if",
	"in", "interface", "is", "null", "object", "package", "return", "super", "this", "throw", "true",
	"try", "typealias", "typeof", "val", "var", "when", "while")

func emitKotlin(e *emitter) string {
	for _, h := range e.header() {
		e.line("// %s", h)
	}
	e.line("")
	if e.opts.pkg != "" {
		e.line("package %s", e.opts.pkg)
		e.line("")
	}
	e.line("import org.json.JSONObject")
	for _, o := range e.objects() {
		e.line("")
		kdoc(e, "", docLines(o.doc))
		e.line("data class %s(", o.name)
		n := fieldNamer{style: camelCase, reserved: kotlinKeywords, escape: func(s string) string { return "`" + s + "`" }}
		names := make([]string, len(o.fields))
		for i, f := range o.fields {
			kdoc(e, "    ", fieldDoc(f))
			names[i] = n.name(f.key)
			t := kotlinType(e, f.typ)
			if f.optional() && !strings.HasSuffix(t, "?") {
				t += "?"
			}
			def := ""
			if !f.required {
				def = " = null"
			}
			e.line("    val %s: %s%s,", names[i], t, def)
		}
		e.line(") {")
		e.line("    companion object {")
		if o.name == e.opts.name {
			e.line("        /** Parses the config's JSON text. */")
			e.line("        fun fromJson(json: String): %s = fromJson(JSONObject(json))", o.name)
			e.line("")
		}
		e.line("        fun fromJson(o: JSONObject): %s = %s(", o.name, o.name)
		for i, f := range o.fields {
			key := quoteKotlin(f.key)
			v := kotlinExpr(f.typ, "o", key, 0)
			if f.optional() && !f.typ.nullable {
				v = "if (o.isNull(" + key + ")) null else " + v
			}
			e.line("            %s = %s,", names[i], v)
		}
		e.line("        )")
		e.line("    }")
		e.line("}")
	}
	return e.b.String()
}

func kdoc(e *emitter, indent string, lines []string) { tsDoc(e, indent, lines) }

func kotlinType(e *emitter, t *tType) string {
	var s string
	switch t.kind {
	case kString:
		s = "String"
	case kInt:
		s = "Long"
	case kNumber:
		s = "Double"
	case kBool:
		s = "Boolean"
	case kArray:
		s = "List<" + kotlinType(e, t.elem) + ">"
	case kMap:
		s = "Map<String, " + kotlinType(e, t.elem) + ">"
	case kObject:
		s = t.obj.name
	case kUnion:
		e.degrade(t, "Kotlin has no type for one of several types; typed as Any (the org.json value)")
		s = "Any"
	default:
		s = "Any"
	}
	if t.nullable {
		s += "?"
	}
	return s
}

// kotlinExpr reads a value of type t from an org.json JSONObject or
// JSONArray (recv) at idx: a quoted key or an index variable.
func kotlinExpr(t *tType, recv, idx string, depth int) string {
	var v string
	switch t.kind {
	case kString:
		v = recv + ".getString(" + idx + ")"
	case kInt:
		v = recv + ".getLong(" + idx + ")"
	case kNumber:
		v = recv + ".getDouble(" + idx + ")"
	case kBool:
		v = recv + ".getBoolean(" + idx + ")"
	case kObject:
		v = t.obj.name + ".fromJson(" + recv + ".getJSONObject(" + idx + "))"
	case kArray:
		a, i := fmt.Sprintf("a%d", depth), fmt.Sprintf("i%d", depth)
		v = fmt.Sprintf("%s.getJSONArray(%s).let { %s -> List(%s.length()) { %s -> %s } }", recv, idx, a, a, i, kotlinExpr(t.elem, a, i, depth+1))
	case kMap:
		m, k := fmt.Sprintf("m%d", depth), fmt.Sprintf("k%d", depth)
		v = fmt.Sprintf("%s.getJSONObject(%s).let { %s -> %s.keys().asSequence().associateWith { %s -> %s } }", recv, idx, m, m, k, kotlinExpr(t.elem, m, k, depth+1))
	default:
		v = recv + ".get(" + idx + ")"
	}
	if t.nullable {
		v = "if (" + recv + ".isNull(" + idx + ")) null else " + v
	}
	return v
}

// ---- Java (records, from a Map any JSON library can produce) ----

var javaKeywords = setOf("_", "abstract", "assert", "boolean", "break", "byte", "case", "catch", "char",
	"class", "const", "continue", "default", "do", "double", "else", "enum", "extends", "false", "final",
	"finally", "float", "for", "goto", "if", "implements", "import", "instanceof", "int", "interface",
	"long", "native", "new", "null", "package", "private", "protected", "public", "return", "short",
	"static", "strictfp", "super", "switch", "synchronized", "this", "throw", "throws", "transient",
	"true", "try", "void", "volatile", "while",
	// Record components can't be named like Object's methods.
	"clone", "finalize", "getClass", "hashCode", "notify", "notifyAll", "toString", "wait", "equals")

func emitJava(e *emitter) string {
	for _, h := range e.header() {
		e.line("// %s", h)
	}
	e.line("")
	if e.opts.pkg != "" {
		e.line("package %s;", e.opts.pkg)
		e.line("")
	}
	for _, imp := range []string{"java.util.ArrayList", "java.util.Collections", "java.util.LinkedHashMap", "java.util.List", "java.util.Map", "java.util.function.Function"} {
		e.line("import %s;", imp)
	}
	objs := e.objects()
	for i, o := range objs {
		indent := ""
		if i > 0 {
			indent = "    "
		}
		e.line("")
		n := fieldNamer{style: camelCase, reserved: javaKeywords, escape: appendUnderscore}
		names := make([]string, len(o.fields))
		for j, f := range o.fields {
			names[j] = n.name(f.key)
		}
		var doc []string
		doc = append(doc, docLines(o.doc)...)
		if i == 0 {
			p := ""
			if len(doc) > 0 {
				doc = append(doc, "")
				p = "<p>"
			}
			doc = append(doc, p+"Build it with {@link #fromMap}, from the config parsed into a {@code Map} by", "your JSON library (Jackson, Gson, ...).")
		}
		var params []string
		for j, f := range o.fields {
			if d := fieldDoc(f); len(d) > 0 {
				params = append(params, "@param "+names[j]+" "+strings.Join(d, " "))
			}
		}
		if len(params) > 0 && len(doc) > 0 {
			doc = append(doc, "")
		}
		doc = append(doc, params...)
		javadoc(e, indent, doc)
		mod := "public "
		e.line("%s%srecord %s(", indent, mod, o.name)
		for j, f := range o.fields {
			sep := ","
			if j == len(o.fields)-1 {
				sep = ") {"
			}
			e.line("%s        %s %s%s", indent, javaType(e, f.typ, f.optional()), names[j], sep)
		}
		e.line("")
		e.line("%s    public static %s fromMap(Map<String, ?> m) {", indent, o.name)
		e.line("%s        return new %s(", indent, o.name)
		for j, f := range o.fields {
			key := quoteJS(f.key)
			var v string
			if f.optional() {
				v = fmt.Sprintf("m.get(%s) == null ? null : %s", key, javaExpr(f.typ, "m.get("+key+")", 0, false))
			} else {
				v = javaExpr(f.typ, "hcRequire(m, "+key+")", 0, false)
			}
			sep := ","
			if j == len(o.fields)-1 {
				sep = ");"
			}
			e.line("%s                %s%s", indent, v, sep)
		}
		e.line("%s    }", indent)
		if i > 0 {
			e.line("    }")
		}
	}
	e.b.WriteString(javaHelpers)
	e.line("}")
	return e.b.String()
}

func javadoc(e *emitter, indent string, lines []string) {
	if len(lines) == 0 {
		return
	}
	e.line("%s/**", indent)
	for _, l := range lines {
		e.line("%s", strings.TrimRight(indent+" * "+javadocEscape.Replace(l), " "))
	}
	e.line("%s */", indent)
}

// javadocEscape keeps a line inside its comment. Java reads \uXXXX escapes
// everywhere, comments included, so "*/" would end it: a backslash
// is written as an HTML entity, which javadoc shows as a backslash.
var javadocEscape = strings.NewReplacer("*/", `*\/`, `\`, "&#92;")

// javaType is a component's type: primitive when the value is always there.
func javaType(e *emitter, t *tType, optional bool) string {
	boxed := optional || t.nullable
	switch t.kind {
	case kString:
		return "String"
	case kInt:
		if boxed {
			return "Long"
		}
		return "long"
	case kNumber:
		if boxed {
			return "Double"
		}
		return "double"
	case kBool:
		if boxed {
			return "Boolean"
		}
		return "boolean"
	case kArray:
		return "List<" + javaType(e, t.elem, true) + ">"
	case kMap:
		return "Map<String, " + javaType(e, t.elem, true) + ">"
	case kObject:
		return t.obj.name
	case kUnion:
		e.degrade(t, "Java has no type for one of several types; typed as Object")
	}
	return "Object"
}

// javaExpr converts v (an Object from the parsed Map) to type t.
func javaExpr(t *tType, v string, depth int, checkNull bool) string {
	var s string
	switch t.kind {
	case kString:
		s = "hcString(" + v + ")"
	case kInt:
		s = "hcLong(" + v + ")"
	case kNumber:
		s = "hcDouble(" + v + ")"
	case kBool:
		s = "hcBoolean(" + v + ")"
	case kObject:
		s = t.obj.name + ".fromMap(hcObject(" + v + "))"
	case kArray, kMap:
		x := fmt.Sprintf("x%d", depth)
		fn := "hcList"
		if t.kind == kMap {
			fn = "hcMap"
		}
		s = fmt.Sprintf("%s(%s, %s -> %s)", fn, v, x, javaExpr(t.elem, x, depth+1, t.elem.nullable))
	default:
		s = v
	}
	if checkNull && t.kind != kAny && t.kind != kUnion {
		s = v + " == null ? null : " + s
	}
	return s
}

const javaHelpers = `
    private static Object hcRequire(Map<String, ?> m, String key) {
        Object v = m.get(key);
        if (v == null) {
            throw new IllegalArgumentException("missing \"" + key + "\"");
        }
        return v;
    }

    private static String hcString(Object v) {
        return (String) v;
    }

    private static long hcLong(Object v) {
        return ((Number) v).longValue();
    }

    private static double hcDouble(Object v) {
        return ((Number) v).doubleValue();
    }

    private static boolean hcBoolean(Object v) {
        return (Boolean) v;
    }

    @SuppressWarnings("unchecked")
    private static Map<String, ?> hcObject(Object v) {
        return (Map<String, ?>) v;
    }

    private static <T> List<T> hcList(Object v, Function<Object, T> f) {
        List<T> out = new ArrayList<>();
        for (Object x : (List<?>) v) {
            out.add(f.apply(x));
        }
        return Collections.unmodifiableList(out);
    }

    private static <T> Map<String, T> hcMap(Object v, Function<Object, T> f) {
        Map<String, T> out = new LinkedHashMap<>();
        for (Map.Entry<String, ?> en : hcObject(v).entrySet()) {
            out.put(en.getKey(), f.apply(en.getValue()));
        }
        return Collections.unmodifiableMap(out);
    }
`

// ---- Python (TypedDict: json.loads output is already the right shape) ----

var pythonKeywords = setOf("False", "None", "True", "and", "as", "assert", "async", "await", "break",
	"class", "continue", "def", "del", "elif", "else", "except", "finally", "for", "from", "global", "if",
	"import", "in", "is", "lambda", "nonlocal", "not", "or", "pass", "raise", "return", "try", "while",
	"with", "yield")

func isPyIdent(s string) bool {
	if s == "" || pythonKeywords[s] {
		return false
	}
	for i, r := range s {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func emitPython(e *emitter) string {
	imports := map[string]bool{"TypedDict": true}
	var body strings.Builder
	w := func(format string, args ...any) {
		fmt.Fprintf(&body, format, args...)
		body.WriteString("\n")
	}
	for _, o := range e.objectsLeavesFirst() {
		classSyntax := true
		for _, f := range o.fields {
			if !isPyIdent(f.key) {
				classSyntax = false
			}
		}
		w("")
		w("")
		if classSyntax {
			w("class %s(TypedDict):", o.name)
			if d := docLines(o.doc); len(d) > 0 {
				if len(d) == 1 {
					w(`    """%s"""`, pyDocEscape(d[0]))
				} else {
					w(`    """%s`, pyDocEscape(d[0]))
					for _, l := range d[1:] {
						w("%s", strings.TrimRight("    "+pyDocEscape(l), " "))
					}
					w(`    """`)
				}
				w("")
			}
			for _, f := range o.fields {
				for _, l := range fieldDoc(f) {
					w("    # %s", l)
				}
				w("    %s: %s", f.key, pyFieldType(e, f, imports))
			}
			continue
		}
		for _, l := range docLines(o.doc) {
			w("# %s", l)
		}
		w("%s = TypedDict(", o.name)
		w("    %s,", quoteJS(o.name))
		w("    {")
		for _, f := range o.fields {
			for _, l := range fieldDoc(f) {
				w("        # %s", l)
			}
			w("        %s: %s,", quoteJS(f.key), pyFieldType(e, f, imports))
		}
		w("    },")
		w(")")
	}
	for _, h := range e.header() {
		e.line("# %s", h)
	}
	if imports["NotRequired"] {
		e.line("# Needs Python 3.11 or later.")
	}
	e.line("")
	e.line("from __future__ import annotations")
	e.line("")
	e.line("from typing import %s", strings.Join(sortedKeys(imports), ", "))
	e.b.WriteString(body.String())
	return e.b.String()
}

func pyDocEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"""`, `\"\"\"`)
}

func pyFieldType(e *emitter, f *tField, imports map[string]bool) string {
	t := pyType(e, f.typ, imports)
	if !f.required {
		imports["NotRequired"] = true
		t = "NotRequired[" + t + "]"
	}
	return t
}

func pyType(e *emitter, t *tType, imports map[string]bool) string {
	var s string
	switch t.kind {
	case kString:
		if len(t.enum) > 0 {
			imports["Literal"] = true
			q := make([]string, len(t.enum))
			for i, v := range t.enum {
				q[i] = quoteJS(v)
			}
			s = "Literal[" + strings.Join(q, ", ") + "]"
		} else {
			s = "str"
		}
	case kInt:
		s = "int"
	case kNumber:
		s = "float"
	case kBool:
		s = "bool"
	case kArray:
		s = "list[" + pyType(e, t.elem, imports) + "]"
	case kMap:
		s = "dict[str, " + pyType(e, t.elem, imports) + "]"
	case kObject:
		s = t.obj.name
	case kUnion:
		vs := make([]string, len(t.variants))
		for i, v := range t.variants {
			vs[i] = pyType(e, v, imports)
		}
		s = strings.Join(vs, " | ")
	default:
		imports["Any"] = true
		return "Any"
	}
	if t.nullable {
		s += " | None"
	}
	return s
}

// ---- PHP (readonly classes with fromArray, for json_decode(..., true)) ----

func emitPHP(e *emitter) string {
	e.line("<?php")
	e.line("")
	for _, h := range e.header() {
		e.line("// %s", h)
	}
	e.line("// Needs PHP 8.1 or later.")
	if e.opts.pkg != "" {
		e.line("")
		e.line("namespace %s;", e.opts.pkg)
	}
	for _, o := range e.objects() {
		e.line("")
		n := fieldNamer{style: camelCase, reserved: setOf("this"), escape: appendUnderscore}
		names := make([]string, len(o.fields))
		for i, f := range o.fields {
			names[i] = n.name(f.key)
		}
		// Required parameters first: PHP deprecates a required one after
		// an optional one. fromArray passes them by name.
		var order []int
		for i, f := range o.fields {
			if f.required {
				order = append(order, i)
			}
		}
		for i, f := range o.fields {
			if !f.required {
				order = append(order, i)
			}
		}
		javadoc(e, "", docLines(o.doc))
		e.line("final class %s", o.name)
		e.line("{")
		var params []string
		for _, i := range order {
			f := o.fields[i]
			doc := strings.Join(fieldDoc(f), " ")
			pt := phpDocType(f.typ)
			if f.optional() && f.typ.kind != kAny && f.typ.kind != kUnion && !f.typ.nullable {
				pt += "|null"
			}
			if doc != "" || f.typ.kind == kArray || f.typ.kind == kMap {
				params = append(params, strings.TrimRight(fmt.Sprintf("@param %s $%s %s", pt, names[i], doc), " "))
			}
		}
		javadoc(e, "    ", params)
		e.line("    public function __construct(")
		for _, i := range order {
			f := o.fields[i]
			t := phpType(e, f.typ)
			if f.optional() && t != "mixed" {
				t = "?" + t
			}
			def := ""
			if !f.required {
				def = " = null"
			}
			e.line("        public readonly %s $%s%s,", t, names[i], def)
		}
		e.line("    ) {")
		e.line("    }")
		e.line("")
		e.line("    /** @param array<string, mixed> $a the config, decoded with json_decode($json, true) */")
		e.line("    public static function fromArray(array $a): self")
		e.line("    {")
		e.line("        return new self(")
		for i, f := range o.fields {
			key := quoteSingle(f.key)
			src := "$a[" + key + "]"
			var v string
			switch {
			case f.optional() && phpNeedsConv(f.typ):
				v = fmt.Sprintf("isset(%s) ? %s : null", src, phpExpr(f.typ, src, 0))
			case f.optional():
				v = src + " ?? null"
			default:
				req := fmt.Sprintf(`(%s ?? throw new \InvalidArgumentException(%s))`, src, quoteSingle(`missing "`+f.key+`"`))
				v = phpExpr(f.typ, req, 0)
				if !phpNeedsConv(f.typ) {
					v = strings.TrimSuffix(strings.TrimPrefix(req, "("), ")")
				}
			}
			e.line("            %s: %s,", names[i], v)
		}
		e.line("        );")
		e.line("    }")
		e.line("}")
	}
	return e.b.String()
}

func phpType(e *emitter, t *tType) string {
	switch t.kind {
	case kString:
		return "string"
	case kInt:
		return "int"
	case kNumber:
		return "float"
	case kBool:
		return "bool"
	case kArray, kMap:
		return "array"
	case kObject:
		return t.obj.name
	case kUnion:
		e.degrade(t, "PHP has no type for one of several types; typed as mixed")
	}
	return "mixed"
}

// phpDocType is the type for a docblock, with what arrays hold.
func phpDocType(t *tType) string {
	var s string
	switch t.kind {
	case kString:
		s = "string"
	case kInt:
		s = "int"
	case kNumber:
		s = "float"
	case kBool:
		s = "bool"
	case kArray:
		s = "list<" + phpDocType(t.elem) + ">"
	case kMap:
		s = "array<string, " + phpDocType(t.elem) + ">"
	case kObject:
		s = t.obj.name
	case kUnion:
		vs := make([]string, len(t.variants))
		for i, v := range t.variants {
			vs[i] = phpDocType(v)
		}
		s = strings.Join(vs, "|")
	default:
		return "mixed"
	}
	if t.nullable {
		s += "|null"
	}
	return s
}

func phpNeedsConv(t *tType) bool {
	switch t.kind {
	case kObject:
		return true
	case kArray, kMap:
		return phpNeedsConv(t.elem)
	}
	return false
}

func phpExpr(t *tType, src string, depth int) string {
	if !phpNeedsConv(t) {
		return src
	}
	var s string
	switch t.kind {
	case kObject:
		s = t.obj.name + "::fromArray(" + src + ")"
	default: // array or map: array_map keeps a map's keys
		x := fmt.Sprintf("$x%d", depth)
		s = fmt.Sprintf("array_map(fn ($%s) => %s, %s)", x[1:], phpExpr(t.elem, x, depth+1), src)
		if t.elem.nullable {
			s = fmt.Sprintf("array_map(fn ($%s) => %s === null ? null : %s, %s)", x[1:], x, phpExpr(t.elem, x, depth+1), src)
		}
	}
	return s
}

// ---- Ruby (Data classes with from_h, for JSON.parse) ----

var rubyReserved = setOf("BEGIN", "END", "alias", "and", "begin", "break", "case", "class", "def",
	"defined", "do", "else", "elsif", "end", "ensure", "false", "for", "if", "in", "module", "next", "nil",
	"not", "or", "redo", "rescue", "retry", "return", "self", "super", "then", "true", "undef", "unless",
	"until", "when", "while", "yield",
	// Methods every Data instance has.
	"hash", "inspect", "members", "method", "methods", "object_id", "send", "tap", "to_h", "to_s", "with",
	"freeze", "frozen", "dup", "clone", "display", "eql", "equal", "instance_variables", "itself")

func emitRuby(e *emitter) string {
	e.line("# frozen_string_literal: true")
	e.line("")
	for _, h := range e.header() {
		e.line("# %s", h)
	}
	e.line("# Needs Ruby 3.2 or later (Data).")
	indent := ""
	if e.opts.pkg != "" {
		e.line("")
		e.line("module %s", e.opts.pkg)
		indent = "  "
	}
	path := func(o *tObject) string {
		if o.name == e.opts.name {
			return o.name
		}
		return e.opts.name + "::" + o.name
	}
	for i, o := range e.objects() {
		if i > 0 || e.opts.pkg == "" {
			e.line("")
		}
		n := fieldNamer{style: snakeCase, reserved: rubyReserved, escape: appendUnderscore}
		names := make([]string, len(o.fields))
		syms := make([]string, len(o.fields))
		for j, f := range o.fields {
			names[j] = n.name(f.key)
			syms[j] = ":" + names[j]
		}
		for _, l := range docLines(o.doc) {
			e.line("%s# %s", indent, l)
		}
		if len(docLines(o.doc)) > 0 {
			e.line("%s#", indent)
		}
		for j, f := range o.fields {
			e.line("%s# @!attribute [r] %s", indent, names[j])
			doc := strings.Join(fieldDoc(f), " ")
			e.line("%s", strings.TrimRight(fmt.Sprintf("%s#   @return [%s] %s", indent, rubyDocType(f.typ, f.optional(), path), doc), " "))
		}
		e.line("%s%s = Data.define(%s) do", indent, path(o), strings.Join(syms, ", "))
		if o.name == e.opts.name {
			e.line("%s  # Builds %s %s from the Hash that JSON.parse returns.", indent, article(o.name), o.name)
		}
		e.line("%s  def self.from_h(h)", indent)
		e.line("%s    new(", indent)
		for j, f := range o.fields {
			key := quoteSingle(f.key)
			var v string
			if f.optional() {
				v = "h[" + key + "]"
				if rubyNeedsConv(f.typ) {
					v += "&.then { |v| " + rubyExpr(f.typ, "v", 0, path) + " }"
				}
			} else {
				v = rubyExpr(f.typ, "h.fetch("+key+")", 0, path)
			}
			sep := ","
			if j == len(o.fields)-1 {
				sep = ""
			}
			e.line("%s      %s: %s%s", indent, names[j], v, sep)
		}
		e.line("%s    )", indent)
		e.line("%s  end", indent)
		e.line("%send", indent)
	}
	if e.opts.pkg != "" {
		e.line("end")
	}
	return e.b.String()
}

func rubyDocType(t *tType, optional bool, path func(*tObject) string) string {
	var s string
	switch t.kind {
	case kString:
		s = "String"
	case kInt:
		s = "Integer"
	case kNumber:
		s = "Float"
	case kBool:
		s = "Boolean"
	case kArray:
		s = "Array<" + rubyDocType(t.elem, false, path) + ">"
	case kMap:
		s = "Hash{String => " + rubyDocType(t.elem, false, path) + "}"
	case kObject:
		s = path(t.obj)
	case kUnion:
		vs := make([]string, len(t.variants))
		for i, v := range t.variants {
			vs[i] = rubyDocType(v, false, path)
		}
		s = strings.Join(vs, ", ")
	default:
		return "Object"
	}
	if optional || t.nullable {
		s += ", nil"
	}
	return s
}

func rubyNeedsConv(t *tType) bool {
	switch t.kind {
	case kObject, kNumber:
		return true
	case kArray, kMap:
		return rubyNeedsConv(t.elem)
	}
	return false
}

func rubyExpr(t *tType, src string, depth int, path func(*tObject) string) string {
	if !rubyNeedsConv(t) {
		return src
	}
	switch t.kind {
	case kObject:
		return path(t.obj) + ".from_h(" + src + ")"
	case kNumber:
		return "Float(" + src + ")"
	}
	x := fmt.Sprintf("x%d", depth)
	inner := rubyExpr(t.elem, x, depth+1, path)
	if t.elem.nullable {
		inner = x + ".nil? ? nil : " + inner
	}
	if t.kind == kMap {
		return fmt.Sprintf("%s.transform_values { |%s| %s }", src, x, inner)
	}
	return fmt.Sprintf("%s.map { |%s| %s }", src, x, inner)
}

// ---- Go (structs for encoding/json) ----

func emitGo(e *emitter) string {
	for _, h := range e.header() {
		e.line("// %s", h)
	}
	e.line("")
	e.line("package %s", e.opts.pkg)
	for _, o := range e.objects() {
		e.line("")
		for _, l := range docLines(o.doc) {
			e.line("// %s", l)
		}
		e.line("type %s struct {", o.name)
		n := fieldNamer{style: func(w []string) string { return pascalCase(w, true) }, escape: appendUnderscore}
		for i, f := range o.fields {
			if i > 0 && len(fieldDoc(f)) > 0 {
				e.line("")
			}
			for _, l := range fieldDoc(f) {
				e.line("\t// %s", l)
			}
			name := n.name(f.key)
			if name[0] < 'A' || name[0] > 'Z' {
				name = "F" + name
			}
			tag := f.key
			if !isGoTagName(tag) {
				e.degrade(f.typ, fmt.Sprintf("the key %q can't be written in a Go struct tag; encoding/json matches %s to it by name, without case", f.key, name))
				tag = ""
			}
			if !f.required {
				tag += ",omitempty"
			}
			t := goType(e, f.typ, f.optional())
			if tag == "" {
				e.line("\t%s %s", name, t)
			} else {
				e.line("\t%s %s `json:%s`", name, t, strconv.Quote(tag))
			}
		}
		e.line("}")
	}
	src := e.b.String()
	if out, err := format.Source([]byte(src)); err == nil {
		return string(out)
	}
	return src
}

// isGoTagName mirrors encoding/json's rule for a name in a struct tag.
func isGoTagName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
		case c > 0x7f && c != utf8.RuneError:
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
		default:
			return false
		}
	}
	return true
}

func goType(e *emitter, t *tType, optional bool) string {
	var s string
	ptr := optional || t.nullable
	switch t.kind {
	case kString:
		s = "string"
	case kInt:
		s = "int64"
	case kNumber:
		s = "float64"
	case kBool:
		s = "bool"
	case kArray:
		return "[]" + goType(e, t.elem, false)
	case kMap:
		return "map[string]" + goType(e, t.elem, false)
	case kObject:
		s = t.obj.name
	case kUnion:
		e.degrade(t, "Go has no type for one of several types; typed as any")
		return "any"
	default:
		return "any"
	}
	if ptr {
		s = "*" + s
	}
	return s
}

// ---- GDScript (Godot 4: classes with from_dict, for JSON.parse_string) ----

var gdReserved = setOf("and", "as", "assert", "await", "break", "breakpoint", "class", "class_name",
	"const", "continue", "elif", "else", "enum", "extends", "false", "for", "func", "if", "in", "is",
	"match", "namespace", "not", "null", "or", "pass", "preload", "return", "self", "signal", "static",
	"super", "trait", "true", "var", "void", "when", "while", "yield", "PI", "TAU", "INF", "NAN",
	// Object's own members.
	"call", "connect", "disconnect", "emit_signal", "free", "get", "get_class", "has_method", "is_class",
	"notification", "script", "set", "to_string", "new")

func emitGDScript(e *emitter) string {
	for _, h := range e.header() {
		e.line("# %s", h)
	}
	e.line("# Needs Godot 4.")
	root := e.opts.name
	usesMap := false
	for i, o := range e.objects() {
		in := ""
		if i == 0 {
			e.line("class_name %s", o.name)
			e.line("extends RefCounted")
		} else {
			e.line("")
			e.line("")
			for _, l := range docLines(o.doc) {
				e.line("## %s", l)
			}
			e.line("class %s:", o.name)
			e.line("\textends RefCounted")
			in = "\t"
		}
		if i == 0 && len(docLines(o.doc)) > 0 {
			e.line("")
			for _, l := range docLines(o.doc) {
				e.line("## %s", l)
			}
		}
		n := fieldNamer{style: snakeCase, reserved: gdReserved, escape: appendUnderscore}
		names := make([]string, len(o.fields))
		e.line("")
		for j, f := range o.fields {
			names[j] = n.name(f.key)
			doc := fieldDoc(f)
			if f.optional() {
				doc = append(doc, gdDocType(f.typ)+", or null when it isn't set.")
			}
			for _, l := range doc {
				e.line("%s## %s", in, l)
			}
			if f.optional() {
				e.line("%svar %s = null", in, names[j])
			} else if t := gdType(f.typ); t == "" {
				e.line("%svar %s", in, names[j])
			} else {
				e.line("%svar %s: %s", in, names[j], t)
			}
		}
		e.line("")
		e.line("")
		if i == 0 {
			e.line("## Builds %s %s from the Dictionary that JSON.parse_string returns.", article(o.name), o.name)
		}
		e.line("%sstatic func from_dict(d: Dictionary) -> %s:", in, o.name)
		e.line("%s\tvar o := %s.new()", in, o.name)
		for j, f := range o.fields {
			key := quoteJS(f.key)
			src := "d[" + key + "]"
			v := gdExpr(f.typ, src, 0, root, &usesMap)
			if f.optional() {
				e.line("%s\tif d.get(%s) != null:", in, key)
				e.line("%s\t\to.%s = %s", in, names[j], v)
			} else if strings.HasPrefix(gdType(f.typ), "Array[") {
				e.line("%s\to.%s.assign(%s)", in, names[j], v)
			} else {
				e.line("%s\to.%s = %s", in, names[j], v)
			}
		}
		e.line("%s\treturn o", in)
	}
	if usesMap {
		e.line("")
		e.line("")
		e.line("## Applies f to every value of a Dictionary.")
		e.line("static func _map_values(d: Dictionary, f: Callable) -> Dictionary:")
		e.line("\tvar out := {}")
		e.line("\tfor k in d:")
		e.line("\t\tout[k] = f.call(d[k])")
		e.line("\treturn out")
	}
	return e.b.String()
}

// gdType is a member's static type, "" for a Variant.
func gdType(t *tType) string {
	if t.nullable {
		return ""
	}
	switch t.kind {
	case kString:
		return "String"
	case kInt:
		return "int"
	case kNumber:
		return "float"
	case kBool:
		return "bool"
	case kObject:
		return t.obj.name
	case kArray:
		switch el := gdType(t.elem); {
		case el == "" || strings.HasPrefix(el, "Array") || el == "Dictionary":
			return "Array"
		default:
			return "Array[" + el + "]"
		}
	case kMap:
		return "Dictionary"
	}
	return ""
}

func gdDocType(t *tType) string {
	switch t.kind {
	case kString:
		return "A String"
	case kInt:
		return "An int"
	case kNumber:
		return "A float"
	case kBool:
		return "A bool"
	case kObject:
		return strings.ToUpper(article(t.obj.name)[:1]) + article(t.obj.name)[1:] + " " + t.obj.name
	case kArray:
		return "An Array"
	case kMap:
		return "A Dictionary"
	}
	return "Any value"
}

func gdNeedsConv(t *tType) bool {
	switch t.kind {
	case kObject, kInt, kNumber:
		return true
	case kArray, kMap:
		return gdNeedsConv(t.elem)
	}
	return false
}

// gdExpr converts src (a value from JSON.parse_string, where every number
// is a float) to type t.
func gdExpr(t *tType, src string, depth int, root string, usesMap *bool) string {
	if !gdNeedsConv(t) {
		return src
	}
	switch t.kind {
	case kObject:
		return t.obj.name + ".from_dict(" + src + ")"
	case kInt:
		return "int(" + src + ")"
	case kNumber:
		return "float(" + src + ")"
	}
	x := fmt.Sprintf("x%d", depth)
	inner := gdExpr(t.elem, x, depth+1, root, usesMap)
	if t.elem.nullable {
		inner = "null if " + x + " == null else " + inner
	}
	if t.kind == kMap {
		*usesMap = true
		return fmt.Sprintf("%s._map_values(%s, func(%s): return %s)", root, src, x, inner)
	}
	return fmt.Sprintf("%s.map(func(%s): return %s)", src, x, inner)
}

// ---- C# (Newtonsoft.Json, as Unity's package and .NET use it) ----

// csMemberNames can't be property names: they'd hide object's members (or,
// on the top-level type, its FromJson).
var csMemberNames = setOf("Equals", "GetHashCode", "GetType", "ToString", "MemberwiseClone", "Finalize", "ReferenceEquals")

// csRefs are the library types the file names, written short unless one of
// the config's own types takes the name.
type csRefs struct {
	list, dict, jtoken, convert, prop, required, exc string
}

func newCSRefs(objects []*tObject) csRefs {
	taken := map[string]bool{}
	for _, o := range objects {
		taken[o.name] = true
	}
	ref := func(short, full string) string {
		if taken[short] {
			return "global::" + full
		}
		return short
	}
	r := csRefs{
		list:     ref("List", "System.Collections.Generic.List"),
		dict:     ref("Dictionary", "System.Collections.Generic.Dictionary"),
		jtoken:   ref("JToken", "Newtonsoft.Json.Linq.JToken"),
		convert:  ref("JsonConvert", "Newtonsoft.Json.JsonConvert"),
		prop:     ref("JsonProperty", "Newtonsoft.Json.JsonProperty"),
		required: ref("Required", "Newtonsoft.Json.Required"),
		exc:      ref("JsonSerializationException", "Newtonsoft.Json.JsonSerializationException"),
	}
	if taken["JsonPropertyAttribute"] {
		r.prop = "global::Newtonsoft.Json.JsonProperty"
	}
	return r
}

func emitCSharp(e *emitter) string {
	for _, h := range e.header() {
		e.line("// %s", h)
	}
	e.line("// Needs Newtonsoft.Json: in Unity, the com.unity.nuget.newtonsoft-json package;")
	e.line("// elsewhere, the Newtonsoft.Json NuGet package.")
	e.line("")
	e.line("#nullable enable")
	e.line("")
	e.line("using System.Collections.Generic;")
	e.line("using Newtonsoft.Json;")
	e.line("using Newtonsoft.Json.Linq;")
	r := newCSRefs(e.objects())
	in := ""
	if e.opts.pkg != "" {
		e.line("")
		e.line("namespace %s", e.opts.pkg)
		e.line("{")
		in = "    "
	}
	for i, o := range e.objects() {
		if i > 0 || e.opts.pkg == "" {
			e.line("")
		}
		csDoc(e, in, docLines(o.doc))
		e.line("%spublic sealed class %s", in, o.name)
		e.line("%s{", in)
		reserved := map[string]bool{o.name: true}
		for k := range csMemberNames {
			reserved[k] = true
		}
		root := o.name == e.opts.name
		if root {
			reserved["FromJson"] = true
		}
		n := fieldNamer{style: func(w []string) string { return pascalCase(w, false) }, reserved: reserved, escape: func(s string) string { return s + "Value" }}
		for j, f := range o.fields {
			if j > 0 {
				e.line("")
			}
			csDoc(e, in+"    ", fieldDoc(f))
			name := n.name(f.key)
			t := csType(e, r, f.typ)
			if f.optional() && !strings.HasSuffix(t, "?") {
				t += "?"
			}
			switch {
			case f.required && !f.typ.nullable:
				e.line("%s    [%s(%s, Required = %s.Always)]", in, r.prop, quoteJS(f.key), r.required)
			case f.required:
				e.line("%s    [%s(%s, Required = %s.AllowNull)]", in, r.prop, quoteJS(f.key), r.required)
			default:
				e.line("%s    [%s(%s)]", in, r.prop, quoteJS(f.key))
			}
			init := ""
			if !strings.HasSuffix(t, "?") && !csValueType(f.typ) {
				// Required.Always means Newtonsoft fills it or throws.
				init = " = null!;"
			}
			e.line("%s    public %s %s { get; set; }%s", in, t, name, init)
		}
		if root {
			if len(o.fields) > 0 {
				e.line("")
			}
			e.line("%s    /// <summary>Parses the config's JSON text. Throws a JsonException when it doesn't match the schema's types or misses a required field.</summary>", in)
			e.line("%s    public static %s FromJson(string json) =>", in, o.name)
			e.line("%s        %s.DeserializeObject<%s>(json) ?? throw new %s(\"the config is null\");", in, r.convert, o.name, r.exc)
		}
		e.line("%s}", in)
	}
	if e.opts.pkg != "" {
		e.line("}")
	}
	return e.b.String()
}

// csDoc writes an XML doc comment.
func csDoc(e *emitter, indent string, lines []string) {
	if len(lines) == 0 {
		return
	}
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	if len(lines) == 1 {
		e.line("%s/// <summary>%s</summary>", indent, esc.Replace(lines[0]))
		return
	}
	e.line("%s/// <summary>", indent)
	for _, l := range lines {
		e.line("%s", strings.TrimRight(indent+"/// "+esc.Replace(l), " "))
	}
	e.line("%s/// </summary>", indent)
}

// csValueType reports whether t is a C# value type (needs ? to be null).
func csValueType(t *tType) bool {
	switch t.kind {
	case kInt, kNumber, kBool:
		return true
	}
	return false
}

func csType(e *emitter, r csRefs, t *tType) string {
	var s string
	switch t.kind {
	case kString:
		s = "string"
	case kInt:
		s = "long"
	case kNumber:
		s = "double"
	case kBool:
		s = "bool"
	case kArray:
		s = r.list + "<" + csType(e, r, t.elem) + ">"
	case kMap:
		s = r.dict + "<string, " + csType(e, r, t.elem) + ">"
	case kObject:
		s = t.obj.name
	case kUnion:
		e.degrade(t, "C# has no type for one of several types; typed as JToken")
		s = r.jtoken
	default:
		s = r.jtoken
	}
	if t.nullable {
		s += "?"
	}
	return s
}
