package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// This file validates tool arguments against a subset of JSON Schema
// (draft 2020-12), using only the standard library. The subset is the one
// OpenAI-compatible providers accept for tool parameters:
//
//	type, properties, required, additionalProperties (boolean), items,
//	enum, minItems, maxItems, uniqueItems, minLength, maxLength, minimum,
//	maximum, format (uuid, date-time, date), and the annotations title,
//	description, default, examples, and $schema.
//
// A schema using any other keyword, such as $ref or oneOf, is rejected when
// the tool is registered, so no rule in a schema is ever silently ignored.

const draft2020 = "https://json-schema.org/draft/2020-12/schema"

var (
	jsonTypes   = []string{"object", "array", "string", "integer", "number", "boolean", "null"}
	formats     = []string{"uuid", "date-time", "date"}
	annotations = []string{"title", "description", "default", "examples", "$schema"}
	keywords    = []string{"type", "properties", "required", "additionalProperties", "items", "enum",
		"minItems", "maxItems", "uniqueItems", "minLength", "maxLength", "minimum", "maximum", "format"}
)

// schema is a compiled schema node.
type schema struct {
	types       []string
	properties  map[string]*schema
	required    []string
	closed      bool // additionalProperties: false
	items       *schema
	enum        []any
	minItems    *int
	maxItems    *int
	uniqueItems bool
	minLength   *int
	maxLength   *int
	minimum     *float64
	maximum     *float64
	format      string
}

// compileSchema parses and checks a tool's parameter schema. The root must
// be an object schema.
func compileSchema(raw json.RawMessage) (*schema, error) {
	doc, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parameters: %w", err)
	}
	obj, ok := doc.(map[string]any)
	if !ok || obj["type"] != "object" {
		return nil, errors.New(`parameters must be a schema with "type": "object"`)
	}
	if v, ok := obj["$schema"]; ok && v != draft2020 {
		return nil, fmt.Errorf("$schema must be %s", draft2020)
	}
	return compileNode(obj, "")
}

func compileNode(obj map[string]any, at string) (*schema, error) {
	fail := func(format string, a ...any) (*schema, error) {
		return nil, fmt.Errorf("schema at %s: %s", pointer(at), fmt.Sprintf(format, a...))
	}
	for k := range obj {
		if !slices.Contains(keywords, k) && !slices.Contains(annotations, k) {
			return fail("keyword %q is not supported", k)
		}
	}
	s := &schema{}
	var err error

	switch t := obj["type"].(type) {
	case nil:
	case string:
		s.types = []string{t}
	case []any:
		for _, v := range t {
			name, ok := v.(string)
			if !ok {
				return fail("type must be a string or a list of strings")
			}
			s.types = append(s.types, name)
		}
	default:
		return fail("type must be a string or a list of strings")
	}
	for _, t := range s.types {
		if !slices.Contains(jsonTypes, t) {
			return fail("unknown type %q", t)
		}
	}

	if v, ok := obj["properties"]; ok {
		props, ok := v.(map[string]any)
		if !ok {
			return fail("properties must be an object")
		}
		s.properties = map[string]*schema{}
		for name, sub := range props {
			subObj, ok := sub.(map[string]any)
			if !ok {
				return fail("property %q must be a schema object", name)
			}
			if s.properties[name], err = compileNode(subObj, at+"/properties/"+name); err != nil {
				return nil, err
			}
		}
	}
	if v, ok := obj["required"]; ok {
		list, ok := v.([]any)
		if !ok {
			return fail("required must be a list of names")
		}
		for _, item := range list {
			name, ok := item.(string)
			if !ok {
				return fail("required must be a list of names")
			}
			s.required = append(s.required, name)
		}
	}
	if v, ok := obj["additionalProperties"]; ok {
		b, ok := v.(bool)
		if !ok {
			return fail("additionalProperties must be true or false")
		}
		s.closed = !b
	}
	if v, ok := obj["items"]; ok {
		sub, ok := v.(map[string]any)
		if !ok {
			return fail("items must be a schema object")
		}
		if s.items, err = compileNode(sub, at+"/items"); err != nil {
			return nil, err
		}
	}
	if v, ok := obj["enum"]; ok {
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			return fail("enum must be a non-empty list")
		}
		for _, item := range list {
			s.enum = append(s.enum, normalize(item))
		}
	}
	if v, ok := obj["uniqueItems"]; ok {
		b, ok := v.(bool)
		if !ok {
			return fail("uniqueItems must be true or false")
		}
		s.uniqueItems = b
	}
	for key, dst := range map[string]**int{"minItems": &s.minItems, "maxItems": &s.maxItems,
		"minLength": &s.minLength, "maxLength": &s.maxLength} {
		if v, ok := obj[key]; ok {
			n, ok := nonNegativeInt(v)
			if !ok {
				return fail("%s must be a non-negative integer", key)
			}
			*dst = &n
		}
	}
	for key, dst := range map[string]**float64{"minimum": &s.minimum, "maximum": &s.maximum} {
		if v, ok := obj[key]; ok {
			n, ok := v.(json.Number)
			f, err := n.Float64()
			if !ok || err != nil {
				return fail("%s must be a number", key)
			}
			*dst = &f
		}
	}
	if v, ok := obj["format"]; ok {
		f, ok := v.(string)
		if !ok || !slices.Contains(formats, f) {
			return fail("format must be one of %s", strings.Join(formats, ", "))
		}
		s.format = f
	}
	return s, nil
}

// validate returns every violation in v, sorted, or nil.
func (s *schema) validate(v any) []string {
	var errs []string
	s.check(v, "", &errs)
	sort.Strings(errs)
	return errs
}

func (s *schema) check(v any, at string, errs *[]string) {
	add := func(format string, a ...any) {
		*errs = append(*errs, fmt.Sprintf("at %s: %s", pointer(at), fmt.Sprintf(format, a...)))
	}
	if len(s.types) > 0 && !slices.ContainsFunc(s.types, func(t string) bool { return isType(v, t) }) {
		add("expected %s, got %s", strings.Join(s.types, " or "), typeOf(v))
		return // the other rules assume the right type
	}
	if s.enum != nil && !slices.ContainsFunc(s.enum, func(e any) bool { return reflect.DeepEqual(e, normalize(v)) }) {
		add("must be one of %s", enumList(s.enum))
	}

	switch val := v.(type) {
	case map[string]any:
		for _, name := range s.required {
			if _, ok := val[name]; !ok {
				add("missing property %q", name)
			}
		}
		for name, item := range val {
			sub, known := s.properties[name]
			switch {
			case known:
				sub.check(item, at+"/"+name, errs)
			case s.closed:
				add("property %q is not allowed", name)
			}
		}
	case []any:
		if s.minItems != nil && len(val) < *s.minItems {
			add("needs at least %d items, has %d", *s.minItems, len(val))
		}
		if s.maxItems != nil && len(val) > *s.maxItems {
			add("allows at most %d items, has %d", *s.maxItems, len(val))
		}
		if s.uniqueItems {
			for i := range val {
				for j := i + 1; j < len(val); j++ {
					if reflect.DeepEqual(normalize(val[i]), normalize(val[j])) {
						add("items %d and %d are equal", i, j)
					}
				}
			}
		}
		if s.items != nil {
			for i, item := range val {
				s.items.check(item, fmt.Sprintf("%s/%d", at, i), errs)
			}
		}
	case string:
		n := utf8.RuneCountInString(val)
		if s.minLength != nil && n < *s.minLength {
			add("must be at least %d characters", *s.minLength)
		}
		if s.maxLength != nil && n > *s.maxLength {
			add("must be at most %d characters", *s.maxLength)
		}
		if s.format != "" && !validFormat(s.format, val) {
			add("is not a valid %s", s.format)
		}
	case json.Number:
		f, _ := val.Float64()
		if s.minimum != nil && f < *s.minimum {
			add("must be at least %v", *s.minimum)
		}
		if s.maximum != nil && f > *s.maximum {
			add("must be at most %v", *s.maximum)
		}
	}
}

// decodeJSON decodes exactly one JSON value, keeping numbers exact.
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
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

func isType(v any, t string) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f) && !math.IsInf(f, 0)
	}
	return false
}

func typeOf(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

// normalize turns numbers into float64 so 1 and 1.0 compare equal, as JSON
// Schema requires.
func normalize(v any) any {
	switch val := v.(type) {
	case json.Number:
		f, _ := val.Float64()
		return f
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = normalize(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			out[k] = normalize(item)
		}
		return out
	}
	return v
}

func nonNegativeInt(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil || i < 0 || i > math.MaxInt32 {
		return 0, false
	}
	return int(i), true
}

func enumList(values []any) string {
	parts := make([]string, len(values))
	for i, v := range values {
		b, _ := json.Marshal(v)
		parts[i] = string(b)
	}
	return strings.Join(parts, ", ")
}

func validFormat(format, s string) bool {
	switch format {
	case "uuid":
		return isUUID(s)
	case "date-time":
		_, err := time.Parse(time.RFC3339, s)
		return err == nil
	case "date":
		_, err := time.Parse(time.DateOnly, s)
		return err == nil
	}
	return false
}

// isUUID accepts the hyphenated form only, as RFC 9562 defines it.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}

func pointer(at string) string {
	if at == "" {
		return "/"
	}
	return at
}
