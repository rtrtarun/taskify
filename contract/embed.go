// Package contract holds Taskify's language-neutral contract
// (docs/architecture.md section 9.2): the OpenAPI document every client is
// generated from, the JSON Schemas of the outbox payloads and the agent
// tools, and the validator for those schemas' subset of JSON Schema.
//
// It imports nothing from the server, so the conformance runner and client
// generators can use it without depending on the reference implementation.
package contract

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
)

// OpenAPI is contract/openapi.yaml: every endpoint, request and response
// body, status code, and the shared error body.
//
//go:embed openapi.yaml
var OpenAPI []byte

//go:embed tools/*.schema.json
var toolFiles embed.FS

//go:embed events/*.schema.json
var eventFiles embed.FS

// ErrNoSchema is returned for a tool or event the contract does not define.
var ErrNoSchema = errors.New("contract: no schema")

const schemaSuffix = ".schema.json"

// ToolSchema returns the parameter schema of the named agent tool, from
// contract/tools/{name}.schema.json: what the model may request. The tool
// registry loads it at start-up (M12-02).
func ToolSchema(name string) (json.RawMessage, error) {
	return readSchema(toolFiles, "tools", name)
}

// ToolNames lists the tools the contract defines, sorted.
func ToolNames() []string { return names(toolFiles, "tools") }

// EventSchema returns the schema of the outbox payload for an event type,
// from contract/events/{type}.schema.json: the body Taskify sends to
// AgentPulse for it, camelCase as AgentPulse expects.
func EventSchema(eventType string) (json.RawMessage, error) {
	return readSchema(eventFiles, "events", eventType)
}

// EventTypes lists the outbox event types the contract defines, sorted.
func EventTypes() []string { return names(eventFiles, "events") }

var (
	eventMu       sync.Mutex
	eventCompiled = map[string]*Schema{}
)

// ValidateEvent checks an outbox payload against its event type's schema
// and returns every violation, or nil.
func ValidateEvent(eventType string, payload []byte) error {
	eventMu.Lock()
	s, ok := eventCompiled[eventType]
	eventMu.Unlock()
	if !ok {
		raw, err := EventSchema(eventType)
		if err != nil {
			return err
		}
		if s, err = CompileSchema(raw); err != nil {
			return fmt.Errorf("contract: events/%s%s: %w", eventType, schemaSuffix, err)
		}
		eventMu.Lock()
		eventCompiled[eventType] = s
		eventMu.Unlock()
	}
	doc, err := DecodeJSON(payload)
	if err != nil {
		return fmt.Errorf("contract: %s payload is not JSON: %w", eventType, err)
	}
	if errs := s.Validate(doc); len(errs) > 0 {
		return fmt.Errorf("contract: %s payload does not match its schema: %s", eventType, strings.Join(errs, "; "))
	}
	return nil
}

func readSchema(files embed.FS, dir, name string) (json.RawMessage, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return nil, fmt.Errorf("%w for %q", ErrNoSchema, name)
	}
	raw, err := files.ReadFile(path.Join(dir, name+schemaSuffix))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w for %q: add contract/%s/%s%s", ErrNoSchema, name, dir, name, schemaSuffix)
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func names(files embed.FS, dir string) []string {
	entries, _ := files.ReadDir(dir) // embedded, so it exists
	var out []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), schemaSuffix); ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}
