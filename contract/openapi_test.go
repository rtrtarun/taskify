package contract_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/rtrtarun/taskify/contract/contracttest"
)

// TestOpenAPILint is the linter for contract/openapi.yaml (M12-01). It runs
// offline: kin-openapi (G19) checks that the document is valid OpenAPI 3.0
// with every reference resolved, and the rules below hold the conventions
// Taskify's clients are generated with.
func TestOpenAPILint(t *testing.T) {
	doc, err := contracttest.Load()
	if err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.0.3" {
		t.Errorf("openapi = %q; the generators in R3 are set up for 3.0.3", doc.OpenAPI)
	}
	snake := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	camelBodies := map[string]bool{ // AgentPulse's bodies, passed through
		"WebhookCallback": true, "LearningSuggestionList": true, "LearningSuggestion": true,
	}
	errorSchema := doc.Components.Schemas["Error"]
	if errorSchema == nil {
		t.Fatal("components.schemas.Error is missing")
	}

	seenIDs := map[string]string{}
	for _, op := range contracttest.Operations(doc) {
		method, path, _ := strings.Cut(op, " ")
		o := doc.Paths.Find(path).GetOperation(method)
		switch {
		case o.OperationID == "":
			t.Errorf("%s has no operationId", op)
		case seenIDs[o.OperationID] != "":
			t.Errorf("%s repeats operationId %s of %s", op, o.OperationID, seenIDs[o.OperationID])
		default:
			seenIDs[o.OperationID] = op
		}
		if len(o.Tags) == 0 {
			t.Errorf("%s has no tag", op)
		}
		if strings.TrimSpace(o.Summary) == "" {
			t.Errorf("%s has no summary", op)
		}
		if o.Responses.Default() != nil {
			t.Errorf("%s has a default response: every status must be listed, so CI can refuse an undocumented one", op)
		}
		ok2xx := false
		for code, ref := range o.Responses.Map() {
			n, err := strconv.Atoi(code)
			if err != nil {
				t.Errorf("%s: response %q is not a status code", op, code)
				continue
			}
			if n >= 200 && n < 300 {
				ok2xx = true
			}
			// Errors under /api/ share one body; /readyz answers 503 with its own.
			if n >= 400 && strings.HasPrefix(path, "/api/") {
				media := ref.Value.Content.Get("application/json")
				if media == nil || media.Schema == nil || media.Schema.Value != errorSchema.Value {
					t.Errorf("%s: %d does not use the Error body", op, n)
				}
			}
			if n == 204 && len(ref.Value.Content) > 0 {
				t.Errorf("%s: 204 has a body", op)
			}
		}
		if !ok2xx {
			t.Errorf("%s has no success response", op)
		}
		authenticated := o.Security == nil || len(*o.Security) > 0
		if authenticated && o.Responses.Status(401) == nil {
			t.Errorf("%s needs credentials but does not list 401", op)
		}
		if authenticated && o.Responses.Status(429) == nil {
			t.Errorf("%s is rate limited but does not list 429", op)
		}
		if o.RequestBody != nil && o.Responses.Status(413) == nil {
			t.Errorf("%s takes a body but does not list 413", op)
		}
	}

	// Field names: snake_case, except AgentPulse's own bodies.
	for name, ref := range doc.Components.Schemas {
		if camelBodies[name] {
			continue
		}
		walkProperties(ref.Value, name, func(at, prop string) {
			if !snake.MatchString(prop) {
				t.Errorf("%s: property %q is not snake_case", at, prop)
			}
		})
	}
}

func walkProperties(s *openapi3.Schema, at string, visit func(at, prop string)) {
	if s == nil {
		return
	}
	for name, p := range s.Properties {
		visit(at, name)
		if p.Ref == "" { // a referenced schema is checked under its own name
			walkProperties(p.Value, at+"."+name, visit)
		}
	}
	if s.Items != nil && s.Items.Ref == "" {
		walkProperties(s.Items.Value, at+"[]", visit)
	}
}
