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
	"sort"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ---- validate ----

// runValidate checks a JSON file without changing anything: always that it
// is one valid JSON value, and with --bin also against the bin's JSON
// Schema, which is downloaded and checked locally. push is still the final
// check.
func runValidate(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("validate")
	binID := fs.String("bin", "", "also check against this bin's JSON Schema: an id, or org-slug/id (needs a secret key)")
	pos, err := exactArgs(fs, args, "<file.json>")
	if err != nil {
		return err
	}
	file, payload, err := a.readPayload(pos[0])
	if err != nil {
		return err
	}
	if err := checkJSON(file, payload); err != nil {
		return err
	}
	p := a.printer()
	if *binID == "" {
		p.ok("%s is valid JSON", file)
		return nil
	}

	c, err := a.authed(common)
	if err != nil {
		return err
	}
	var settings struct {
		ID        string          `json:"id"`
		HasSchema bool            `json:"has_schema"`
		Schema    json.RawMessage `json:"schema"`
	}
	if err := c.doJSON(ctx, http.MethodGet, binPath(*binID, "/settings"), nil, &settings); err != nil {
		return err
	}
	if !settings.HasSchema || len(settings.Schema) == 0 {
		p.ok("%s is valid JSON (%s has no schema)", file, *binID)
		return nil
	}
	errs, err := validateSchema(settings.Schema, payload)
	if err != nil {
		return err
	}
	if len(errs) > 0 {
		a.printSchemaErrorList(file, *binID, payload, errs, "")
		return errReported
	}
	p.ok("%s matches the schema of %s", file, *binID)
	return nil
}

// readPayload reads a JSON file, or stdin for "-". It returns the name to
// show for it.
func (a *App) readPayload(file string) (string, []byte, error) {
	var b []byte
	var err error
	if file == "-" {
		file = "<stdin>"
		b, err = io.ReadAll(a.Stdin)
	} else {
		b, err = os.ReadFile(file)
	}
	if err != nil {
		return file, nil, fmt.Errorf("read %s: %w", file, err)
	}
	return file, b, nil
}

// schemaResourceURL names the downloaded schema while it is compiled;
// nothing is ever loaded from it.
const schemaResourceURL = "mem://bin-schema.json"

// validateSchema checks payload against a JSON Schema (draft 2020-12,
// unless the schema's $schema names another draft), with "format" checked.
// A $ref may only point inside the schema: nothing is fetched or read from
// disk. It returns the failures, one per failing rule, ordered by location
// in the payload.
func validateSchema(schema, payload []byte) ([]schemaError, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, fmt.Errorf("the bin's schema is invalid: %s", compileErrorMessage(err))
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noLoader{})
	c.AssertFormat()
	if err := c.AddResource(schemaResourceURL, doc); err != nil {
		return nil, fmt.Errorf("the bin's schema is invalid: %s", compileErrorMessage(err))
	}
	s, err := c.Compile(schemaResourceURL)
	if err != nil {
		return nil, fmt.Errorf("the bin's schema is invalid: %s", compileErrorMessage(err))
	}
	v, err := decodeOne(payload)
	if err != nil {
		return nil, err
	}
	err = s.Validate(v)
	if err == nil {
		return nil, nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return nil, err
	}
	var out []schemaError
	leafErrors(ve.DetailedOutput(), &out)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].InstanceLocation != out[j].InstanceLocation {
			return out[i].InstanceLocation < out[j].InstanceLocation
		}
		return out[i].KeywordLocation < out[j].KeywordLocation
	})
	return out, nil
}

// leafErrors turns the failed rules into schemaErrors.
func leafErrors(u *jsonschema.OutputUnit, out *[]schemaError) {
	var leaves []jsonschema.OutputUnit
	outputLeaves(u, &leaves)
	for _, l := range leaves {
		*out = append(*out, schemaError{InstanceLocation: l.InstanceLocation, KeywordLocation: l.KeywordLocation, Message: leafMessage(l)})
	}
}

// noLoader refuses every URL a $ref or $schema names outside the schema.
type noLoader struct{}

func (noLoader) Load(string) (any, error) {
	return nil, errors.New("only references inside the schema are supported")
}

// decodeOne decodes exactly one JSON value, keeping numbers exact
// (json.Number), so large integers are checked as written.
func decodeOne(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return v, nil
}
