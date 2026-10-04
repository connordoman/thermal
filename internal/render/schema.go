package render

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// SchemaJSON is the JSON Schema for block documents (POST /v1/print/json).
//
//go:embed schema/job.v1.json
var SchemaJSON []byte

const schemaURL = "https://thermal.local/schema/job.v1.json"

var schemas = sync.OnceValues(func() (map[string]*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(SchemaJSON))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaURL, doc); err != nil {
		return nil, err
	}
	out := map[string]*jsonschema.Schema{}
	for _, def := range []string{"document", "blocks"} {
		s, err := c.Compile(schemaURL + "#/$defs/" + def)
		if err != nil {
			return nil, err
		}
		out[def] = s
	}
	return out, nil
})

// SchemaError lists the ways a document breaks the schema.
type SchemaError struct {
	Problems []Problem
}

// Problem is one schema violation.
type Problem struct {
	Path    string `json:"path"` // JSON pointer into the document
	Message string `json:"message"`
}

func (e *SchemaError) Error() string {
	var b strings.Builder
	b.WriteString("document does not match the schema")
	for i, p := range e.Problems {
		if i == 5 {
			fmt.Fprintf(&b, "; and %d more", len(e.Problems)-5)
			break
		}
		fmt.Fprintf(&b, "; %s: %s", p.Path, p.Message)
	}
	return b.String()
}

// Validate checks body against the schema.
func Validate(body []byte) error {
	s, err := schemas()
	if err != nil {
		return fmt.Errorf("loading schema: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return &SchemaError{[]Problem{{"", "invalid JSON: " + err.Error()}}}
	}
	sch := s["document"]
	if _, ok := inst.([]any); ok {
		sch = s["blocks"]
	}
	err = sch.Validate(inst)
	var ve *jsonschema.ValidationError
	if errors.As(err, &ve) {
		return &SchemaError{problems(ve)}
	}
	return err
}

var printer = message.NewPrinter(language.English)

// problems flattens a validation error to its leaves, dropping the noise
// from the if/then dispatch on block type.
func problems(ve *jsonschema.ValidationError) []Problem {
	var out []Problem
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			out = append(out, Problem{
				Path:    "/" + strings.Join(e.InstanceLocation, "/"),
				Message: e.ErrorKind.LocalizedString(printer),
			})
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	slices.SortStableFunc(out, func(a, b Problem) int { return strings.Compare(a.Path, b.Path) })
	return slices.CompactFunc(out, func(a, b Problem) bool { return a == b })
}

// decodeStrict decodes JSON already checked against the schema.
func decodeStrict(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
