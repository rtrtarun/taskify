// Package contracttest loads contract/openapi.yaml for tests: the linter,
// the comparison of documented and served routes, and the validation of
// responses against the document (M12-01, M12-05). It uses kin-openapi
// (G19) and imports no server package, so the conformance runner can use it
// too.
package contracttest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/rtrtarun/taskify/contract"
)

// Load parses and validates contract/openapi.yaml. Each call returns a new
// document, which the caller may change.
func Load() (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(contract.OpenAPI)
	if err != nil {
		return nil, fmt.Errorf("contract: openapi.yaml: %w", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("contract: openapi.yaml is not a valid OpenAPI document: %w", err)
	}
	return doc, nil
}

var (
	shared     *openapi3.T
	sharedErr  error
	sharedOnce sync.Once
)

// Doc returns the document, loaded once per process. Callers must not
// change it.
func Doc() (*openapi3.T, error) {
	sharedOnce.Do(func() { shared, sharedErr = Load() })
	return shared, sharedErr
}

// Operations lists the documented operations as "METHOD /path", sorted,
// with path parameters written as in the document ({id}).
func Operations(doc *openapi3.T) []string {
	var out []string
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			out = append(out, strings.ToUpper(method)+" "+path)
		}
	}
	slices.Sort(out)
	return out
}
