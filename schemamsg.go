package cli

// Wording for schema errors. Messages are built from each error's kind and
// data, in hotcfg's words, rather than taken from the validator library, so
// they stay the same whatever library version does the checking.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// schemaErrorMessage words one failed rule. The value it is about is named
// separately (instance_location), so messages say what the value must be.
func schemaErrorMessage(k jsonschema.ErrorKind) string {
	switch k := k.(type) {
	case *kind.Type:
		want := make([]string, len(k.Want))
		for i, t := range k.Want {
			want[i] = withArticle(t)
		}
		return fmt.Sprintf("must be %s, not %s", strings.Join(want, " or "), withArticle(k.Got))
	case *kind.Enum:
		if len(k.Want) == 1 {
			return "must be " + jsonValue(k.Want[0])
		}
		want := make([]string, len(k.Want))
		for i, v := range k.Want {
			want[i] = jsonValue(v)
		}
		return "must be one of " + strings.Join(want, ", ")
	case *kind.Const:
		return "must be " + jsonValue(k.Want)
	case *kind.Format:
		return "must be a valid " + k.Want
	case *kind.Required:
		return fmt.Sprintf("is missing the required %s %s", plural(len(k.Missing), "property", "properties"), quoteList(k.Missing))
	case *kind.DependentRequired:
		return fmt.Sprintf("needs %s %s, because it has %s", plural(len(k.Missing), "property", "properties"), quoteList(k.Missing), quote(k.Prop))
	case *kind.Dependency:
		return fmt.Sprintf("needs %s %s, because it has %s", plural(len(k.Missing), "property", "properties"), quoteList(k.Missing), quote(k.Prop))
	case *kind.AdditionalProperties:
		return fmt.Sprintf("has %s %s, which the schema doesn't allow", plural(len(k.Properties), "property", "properties"), quoteList(k.Properties))
	case *kind.PropertyNames:
		return fmt.Sprintf("has a property name the schema doesn't allow: %s", quote(k.Property))
	case *kind.FalseSchema:
		return "isn't allowed here by the schema"
	case *kind.Not:
		return `must not match the schema under "not"`
	case *kind.OneOf:
		if len(k.Subschemas) >= 2 {
			return fmt.Sprintf(`must match exactly one "oneOf" option, but matches options %d and %d`, k.Subschemas[0], k.Subschemas[1])
		}
		return `must match exactly one "oneOf" option`
	case *kind.AnyOf:
		return `must match at least one "anyOf" option`
	case *kind.AllOf:
		return `must match every "allOf" schema`
	case *kind.MinProperties:
		return fmt.Sprintf("must have at least %s, has %d", count(k.Want, "property", "properties"), k.Got)
	case *kind.MaxProperties:
		return fmt.Sprintf("must have at most %s, has %d", count(k.Want, "property", "properties"), k.Got)
	case *kind.MinItems:
		return fmt.Sprintf("must have at least %s, has %d", count(k.Want, "item", "items"), k.Got)
	case *kind.MaxItems:
		return fmt.Sprintf("must have at most %s, has %d", count(k.Want, "item", "items"), k.Got)
	case *kind.AdditionalItems:
		return fmt.Sprintf("has %s more than the schema allows", count(k.Count, "item", "items"))
	case *kind.UniqueItems:
		return fmt.Sprintf("must not repeat items: items %d and %d are equal", k.Duplicates[0], k.Duplicates[1])
	case *kind.Contains:
		return `must have an item that matches "contains"`
	case *kind.MinContains:
		return fmt.Sprintf(`must have at least %s matching "contains", has %d`, count(k.Want, "item", "items"), len(k.Got))
	case *kind.MaxContains:
		return fmt.Sprintf(`must have at most %s matching "contains", has %d`, count(k.Want, "item", "items"), len(k.Got))
	case *kind.MinLength:
		return fmt.Sprintf("must be at least %s long, is %d", count(k.Want, "character", "characters"), k.Got)
	case *kind.MaxLength:
		return fmt.Sprintf("must be at most %s long, is %d", count(k.Want, "character", "characters"), k.Got)
	case *kind.Pattern:
		return "must match the pattern " + quote(k.Want)
	case *kind.Minimum:
		return fmt.Sprintf("must be at least %s, is %s", number(k.Want), number(k.Got))
	case *kind.Maximum:
		return fmt.Sprintf("must be at most %s, is %s", number(k.Want), number(k.Got))
	case *kind.ExclusiveMinimum:
		return fmt.Sprintf("must be greater than %s, is %s", number(k.Want), number(k.Got))
	case *kind.ExclusiveMaximum:
		return fmt.Sprintf("must be less than %s, is %s", number(k.Want), number(k.Got))
	case *kind.MultipleOf:
		return fmt.Sprintf("must be a multiple of %s, is %s", number(k.Want), number(k.Got))
	case *kind.ContentEncoding:
		return "must be valid " + k.Want
	case *kind.ContentMediaType:
		return "must be valid " + k.Want
	case *kind.ContentSchema:
		return `must have content that matches "contentSchema"`
	}
	return "doesn't match the schema"
}

// compileErrorMessage words why a schema can't be used. Locations are
// JSON Pointers into the schema.
func compileErrorMessage(err error) string {
	var (
		syntax  *json.SyntaxError
		invalid *jsonschema.SchemaValidationError
		load    *jsonschema.LoadURLError
		notFnd  *jsonschema.JSONPointerNotFoundError
		badPtr  *jsonschema.InvalidJsonPointerError
		anchor  *jsonschema.AnchorNotFoundError
		regex   *jsonschema.InvalidRegexError
		draft   *jsonschema.UnsupportedDraftError
		meta    *jsonschema.InvalidMetaSchemaURLError
		dupID   *jsonschema.DuplicateIDError
		dupAnc  *jsonschema.DuplicateAnchorError
		parseID *jsonschema.ParseIDError
		parseAn *jsonschema.ParseAnchorError
		vocab   *jsonschema.UnsupportedVocabularyError
	)
	switch {
	case errors.As(err, &syntax):
		return "not valid JSON: " + syntax.Error()
	case errors.As(err, &invalid):
		var ve *jsonschema.ValidationError
		if errors.As(invalid.Err, &ve) {
			var leaves []jsonschema.OutputUnit
			outputLeaves(ve.DetailedOutput(), &leaves)
			if len(leaves) > 0 {
				msg := fmt.Sprintf("%s %s", schemaPointer(leaves[0].InstanceLocation), leafMessage(leaves[0]))
				if n := len(leaves) - 1; n > 0 {
					msg += fmt.Sprintf(" (and %d more)", n)
				}
				return msg
			}
		}
		return "not valid JSON Schema"
	case errors.As(err, &load):
		return fmt.Sprintf("refers to %s, outside the schema; references may only point inside it", quote(displayURL(load.URL)))
	case errors.As(err, &notFnd):
		return fmt.Sprintf("a $ref points to %s, which isn't in the schema", schemaPointer(fragment(notFnd.URL)))
	case errors.As(err, &badPtr):
		return fmt.Sprintf("a $ref has an invalid JSON Pointer: %s", quote(fragment(badPtr.URL)))
	case errors.As(err, &anchor):
		return fmt.Sprintf("a $ref names the anchor %s, which the schema doesn't define", quote(displayURL(anchor.Reference)))
	case errors.As(err, &regex):
		return fmt.Sprintf("%s is not a valid regular expression: %s", schemaPointer(fragment(regex.URL)), quote(regex.Regex))
	case errors.As(err, &draft), errors.As(err, &meta):
		return "$schema is not a supported JSON Schema draft"
	case errors.As(err, &dupID):
		return fmt.Sprintf("the $id %s is defined twice", quote(displayURL(dupID.ID)))
	case errors.As(err, &dupAnc):
		return fmt.Sprintf("the anchor %s is defined twice", quote(dupAnc.Anchor))
	case errors.As(err, &parseID):
		return fmt.Sprintf("%s has an invalid $id", schemaPointer(fragment(parseID.URL)))
	case errors.As(err, &parseAn):
		return fmt.Sprintf("%s has an invalid anchor", schemaPointer(fragment(parseAn.URL)))
	case errors.As(err, &vocab):
		return "uses a vocabulary that isn't supported"
	}
	return "not valid JSON Schema"
}

// outputLeaves collects the leaves of a validation output tree: they name
// the rules that failed; the inner nodes only say that a subschema did.
func outputLeaves(u *jsonschema.OutputUnit, out *[]jsonschema.OutputUnit) {
	if len(u.Errors) == 0 {
		*out = append(*out, *u)
		return
	}
	for i := range u.Errors {
		outputLeaves(&u.Errors[i], out)
	}
}

func leafMessage(u jsonschema.OutputUnit) string {
	if u.Error == nil {
		return "doesn't match the schema"
	}
	return schemaErrorMessage(u.Error.Kind)
}

// schemaPointer names a place in the schema.
func schemaPointer(ptr string) string {
	if ptr == "" {
		return "the root"
	}
	return quote(ptr)
}

// displayURL shows a URL the schema refers to as the schema wrote it: a
// relative reference is resolved against the name the schema is compiled
// under, which means nothing to its author.
func displayURL(u string) string {
	if rest, ok := strings.CutPrefix(u, schemaResourceURL); ok {
		return strings.TrimPrefix(rest, "/")
	}
	return u
}

// fragment is the JSON Pointer part of a schema URL ("x#/a/b" → "/a/b").
func fragment(u string) string {
	_, f, _ := strings.Cut(u, "#")
	return f
}

func withArticle(jsonType string) string {
	switch jsonType {
	case "null":
		return "null"
	case "integer", "object", "array":
		return "an " + jsonType
	}
	return "a " + jsonType // string, number, boolean
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func count(n int, one, many string) string {
	return strconv.Itoa(n) + " " + plural(n, one, many)
}

// quote shows a name or string as JSON, so control characters are escaped.
func quote(s string) string { return jsonValue(s) }

// quoteList shows names sorted, so a message doesn't depend on map order.
func quoteList(names []string) string {
	sorted := slices.Sorted(slices.Values(names))
	q := make([]string, len(sorted))
	for i, n := range sorted {
		q[i] = quote(n)
	}
	return strings.Join(q, ", ")
}

// maxValueLen bounds a value shown in a message.
const maxValueLen = 80

// jsonValue shows a value as compact JSON, shortened if long.
func jsonValue(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "the given value"
	}
	s := strings.TrimSuffix(buf.String(), "\n")
	if len(s) > maxValueLen {
		r := []rune(s)
		if len(r) > maxValueLen {
			s = string(r[:maxValueLen-1]) + "…"
		}
	}
	return s
}

// number shows an exact number: integers in full, decimals without float
// rounding where they terminate.
func number(r *big.Rat) string {
	if r == nil {
		return "?"
	}
	if r.IsInt() {
		return r.Num().String()
	}
	for prec := 1; prec <= 20; prec++ {
		s := r.FloatString(prec)
		if back, ok := new(big.Rat).SetString(s); ok && back.Cmp(r) == 0 {
			return s
		}
	}
	return strings.TrimRight(r.FloatString(20), "0")
}
