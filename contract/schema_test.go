package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustCompile(t *testing.T, raw string) *Schema {
	t.Helper()
	s, err := CompileSchema(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("compile %s: %v", raw, err)
	}
	return s
}

func errsFor(t *testing.T, s *Schema, instance string) []string {
	t.Helper()
	v, err := DecodeJSON([]byte(instance))
	if err != nil {
		t.Fatalf("instance %s: %v", instance, err)
	}
	return s.Validate(v)
}

func TestSchemaKeywords(t *testing.T) {
	s := mustCompile(t, `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"title": "Example",
		"properties": {
			"id":       {"type": "string", "format": "uuid", "description": "a task"},
			"due":      {"type": "string", "format": "date-time"},
			"day":      {"type": "string", "format": "date"},
			"name":     {"type": "string", "minLength": 2, "maxLength": 4},
			"minutes":  {"type": "integer", "minimum": 1, "maximum": 600},
			"score":    {"type": "number"},
			"flag":     {"type": "boolean"},
			"note":     {"type": ["string", "null"]},
			"level":    {"enum": ["low", "high", 3]},
			"tags":     {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 2, "uniqueItems": true},
			"nested":   {"type": "object", "properties": {"x": {"type": "integer"}}, "required": ["x"], "additionalProperties": false}
		},
		"required": ["id"],
		"additionalProperties": false
	}`)

	valid := `{"id": "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", "due": "2026-11-21T09:00:00Z", "day": "2026-11-21",
		"name": "ab", "minutes": 600, "score": 1.5, "flag": true, "note": null, "level": 3.0,
		"tags": ["a", "b"], "nested": {"x": 1}}`
	if errs := errsFor(t, s, valid); errs != nil {
		t.Fatalf("valid instance rejected: %v", errs)
	}
	// 2.0 is an integer in JSON Schema; multi-byte names count characters.
	if errs := errsFor(t, s, `{"id": "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", "minutes": 2.0, "name": "éé"}`); errs != nil {
		t.Errorf("rejected: %v", errs)
	}

	for instance, want := range map[string]string{
		`{}`:                      `missing property "id"`,
		`{"id": "x", "extra": 1}`: `property "extra" is not allowed`,
		`{"id": "not-a-uuid"}`:    `at /id: is not a valid uuid`,
		`{"id": "0193a1b2c3d47e5f8a9b0c1d2e3f4a5b"}`: `is not a valid uuid`,
		`{"id": 5}`:                               `at /id: expected string, got number`,
		`{"id": "x", "due": "tomorrow"}`:          `is not a valid date-time`,
		`{"id": "x", "day": "21/11/2026"}`:        `is not a valid date`,
		`{"id": "x", "name": "a"}`:                `at least 2 characters`,
		`{"id": "x", "name": "abcde"}`:            `at most 4 characters`,
		`{"id": "x", "minutes": 1.5}`:             `expected integer`,
		`{"id": "x", "minutes": 0}`:               `must be at least 1`,
		`{"id": "x", "minutes": 601}`:             `must be at most 600`,
		`{"id": "x", "flag": "yes"}`:              `expected boolean`,
		`{"id": "x", "note": 1}`:                  `expected string or null`,
		`{"id": "x", "level": "mid"}`:             `must be one of "low", "high", 3`,
		`{"id": "x", "tags": []}`:                 `at least 1 items`,
		`{"id": "x", "tags": ["a","b","c"]}`:      `at most 2 items`,
		`{"id": "x", "tags": ["a","a"]}`:          `items 0 and 1 are equal`,
		`{"id": "x", "tags": ["a", 1]}`:           `at /tags/1: expected string`,
		`{"id": "x", "nested": {}}`:               `at /nested: missing property "x"`,
		`{"id": "x", "nested": {"x": 1, "y": 2}}`: `at /nested: property "y" is not allowed`,
	} {
		errs := errsFor(t, s, instance)
		if !strings.Contains(strings.Join(errs, "; "), want) {
			t.Errorf("%s: errors = %v, want %q", instance, errs, want)
		}
	}

	// Every violation is reported, not only the first.
	if errs := errsFor(t, s, `{"extra": 1, "flag": 2}`); len(errs) != 3 {
		t.Errorf("errors = %v, want 3", errs)
	}
	// Properties not listed are allowed unless additionalProperties is false.
	open := mustCompile(t, `{"type": "object", "properties": {"a": {"type": "string"}}}`)
	if errs := errsFor(t, open, `{"b": 1}`); errs != nil {
		t.Errorf("open object: %v", errs)
	}
}

func TestSchemaRejectsUnsupportedOrInvalid(t *testing.T) {
	for name, raw := range map[string]string{
		"$ref":                        `{"type": "object", "properties": {"a": {"$ref": "#/$defs/x"}}}`,
		"oneOf":                       `{"type": "object", "oneOf": [{"required": ["a"]}]}`,
		"pattern":                     `{"type": "object", "properties": {"a": {"type": "string", "pattern": "^x"}}}`,
		"additionalProperties schema": `{"type": "object", "additionalProperties": {"type": "string"}}`,
		"unknown type":                `{"type": "object", "properties": {"a": {"type": "text"}}}`,
		"type not a string":           `{"type": "object", "properties": {"a": {"type": 5}}}`,
		"unknown format":              `{"type": "object", "properties": {"a": {"type": "string", "format": "email"}}}`,
		"negative minItems":           `{"type": "object", "properties": {"a": {"type": "array", "minItems": -1}}}`,
		"empty enum":                  `{"type": "object", "properties": {"a": {"enum": []}}}`,
		"required not a list":         `{"type": "object", "required": "a"}`,
		"older draft":                 `{"$schema": "http://json-schema.org/draft-07/schema#", "type": "object"}`,
		"root not an object":          `{"type": "string"}`,
		"not JSON":                    `{"type": `,
	} {
		if _, err := CompileSchema(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: schema accepted", name)
		}
	}
	// The error names the offending keyword and where it is.
	_, err := CompileSchema(json.RawMessage(`{"type": "object", "properties": {"a": {"oneOf": []}}}`))
	if err == nil || !strings.Contains(err.Error(), `"oneOf"`) || !strings.Contains(err.Error(), "/properties/a") {
		t.Errorf("error = %v", err)
	}
}

func TestDecodeJSON(t *testing.T) {
	for _, bad := range []string{``, `{`, `{} {}`, `{}x`} {
		if _, err := DecodeJSON([]byte(bad)); err == nil {
			t.Errorf("%q decoded", bad)
		}
	}
	v, err := DecodeJSON([]byte(` {"n": 12345678901234567890} `))
	if err != nil || v.(map[string]any)["n"].(json.Number).String() != "12345678901234567890" {
		t.Errorf("decode = %v, %v", v, err)
	}
}
