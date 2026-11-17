package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
)

// ---- schema infer ----

// runSchemaInfer writes a JSON Schema that accepts every given JSON file,
// as a starting point for a bin's schema. The dashboard's "Generate from
// payloads" follows the same rules (web/src/lib/inferSchema.ts in hotcfg):
//
//   - every value gets its type; a whole number is "integer", any other
//     number "number" (integer and number together merge to "number");
//   - an object lists every key under properties, in document order, and
//     all of them under required; other keys stay allowed;
//   - an array's items is one schema merged from all its elements: keys of
//     objects are united, and required keeps only the keys every element
//     has; differing scalar types become a list of types, anything else
//     anyOf; an empty array gets no items;
//   - several files merge the same way as array elements.
func runSchemaInfer(ctx context.Context, a *App, args []string) error {
	fs, _ := a.newFlagSet("schema infer")
	out := fs.String("o", "", "write the schema to this file instead of stdout")
	fs.StringVar(out, "out", "", "same as -o")
	files, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return usagef("missing <file.json> (use - to read standard input)")
	}
	root := shape{}
	for _, f := range files {
		var b []byte
		if f == "-" {
			b, err = io.ReadAll(a.Stdin)
		} else {
			b, err = os.ReadFile(f)
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		v, err := parseOrdered(b)
		if err != nil {
			return fmt.Errorf("%s is not valid JSON: %w", f, err)
		}
		root.add(v)
	}
	schema := append(ordered{{"$schema", "https://json-schema.org/draft/2020-12/schema"}}, root.schema()...)
	raw, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return err
	}
	pretty.WriteByte('\n')
	if *out == "" || *out == "-" {
		_, err = a.Stdout.Write(pretty.Bytes())
		return err
	}
	if err := os.WriteFile(*out, pretty.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Fprintf(a.Stderr, "Wrote %s. Review and tighten it (enums, minimums, optional keys), then paste it into the bin's Schema tab, or create a bin with it: hotcfg bins create <bin_id> --schema %s\n", *out, *out)
	return nil
}

// ordered is a JSON object to write, with its keys in order.
type ordered []kv

type kv struct {
	k string
	v any
}

func (o ordered) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(e.k)
		v, err := json.Marshal(e.v)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// kindOf is a value's JSON Schema type. Like JavaScript, a number is an
// integer when its float64 value is whole and finite.
func kindOf(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		f, err := strconv.ParseFloat(string(v), 64)
		if err == nil && !math.IsInf(f, 0) && f == math.Trunc(f) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case *oMap:
		return "object"
	}
	panic(fmt.Sprintf("not a JSON value: %T", v))
}

// shape is what's been seen at one place in the examples: one entry per
// kind, in order of first appearance.
type shape []*seen

type seen struct {
	kind   string
	keys   []string          // objects: keys in order of first appearance
	props  map[string]*shape // objects
	counts map[string]int    // objects: how many examples had each key
	n      int               // objects: how many examples were objects
	items  *shape            // arrays: nil until an element is seen
}

func (s *shape) find(kind string) *seen {
	for _, e := range *s {
		if e.kind == kind {
			return e
		}
	}
	return nil
}

func (s *shape) add(v any) {
	kind := kindOf(v)
	if kind == "integer" && s.find("number") != nil {
		kind = "number"
	}
	if kind == "number" {
		if e := s.find("integer"); e != nil {
			e.kind = "number"
		}
	}
	e := s.find(kind)
	if e == nil {
		e = &seen{kind: kind}
		*s = append(*s, e)
	}
	switch v := v.(type) {
	case *oMap:
		if e.props == nil {
			e.props, e.counts = map[string]*shape{}, map[string]int{}
		}
		e.n++
		for _, k := range v.keys {
			c := e.props[k]
			if c == nil {
				c = &shape{}
				e.props[k] = c
				e.keys = append(e.keys, k)
			}
			c.add(v.vals[k])
			e.counts[k]++
		}
	case []any:
		for _, el := range v {
			if e.items == nil {
				e.items = &shape{}
			}
			e.items.add(el)
		}
	}
}

func (s *shape) schema() ordered {
	var parts []ordered
	scalars := true
	for _, e := range *s {
		switch e.kind {
		case "object":
			scalars = false
			props := ordered{}
			var required []string
			for _, k := range e.keys {
				props = append(props, kv{k, e.props[k].schema()})
				if e.counts[k] == e.n {
					required = append(required, k)
				}
			}
			o := ordered{{"type", "object"}, {"properties", props}}
			if len(required) > 0 {
				o = append(o, kv{"required", required})
			}
			parts = append(parts, o)
		case "array":
			scalars = false
			o := ordered{{"type", "array"}}
			if e.items != nil {
				o = append(o, kv{"items", e.items.schema()})
			}
			parts = append(parts, o)
		default:
			parts = append(parts, ordered{{"type", e.kind}})
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	if scalars {
		types := make([]string, len(parts))
		for i, p := range parts {
			types[i] = p[0].v.(string)
		}
		return ordered{{"type", types}}
	}
	return ordered{{"anyOf", parts}}
}
