package contracttest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// LegacyPrefix is where the original Taskify's task routes are served as
// aliases of /api/v1/tasks. The contract documents them once, under /api/v1.
const LegacyPrefix = "/tasks"

// Validator checks responses against contract/openapi.yaml (M12-05).
//
// It is stricter than the document reads, in one way: an object schema
// that lists properties and says nothing about others is treated as closed,
// so a response field the contract does not describe fails. The published
// document leaves such objects open, because clients must ignore fields
// they do not know (section 9.2); the server must not send them.
type Validator struct {
	doc    *openapi3.T
	router routers.Router
	errs   *openapi3.Schema
}

// NewValidator loads the contract for response validation.
func NewValidator() (*Validator, error) {
	doc, err := Load()
	if err != nil {
		return nil, err
	}
	closeObjects(doc)
	// Match requests on their path whatever host they were sent to.
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("contract: router: %w", err)
	}
	errSchema := doc.Components.Schemas["Error"]
	if errSchema == nil {
		return nil, errors.New("contract: components.schemas.Error is missing")
	}
	return &Validator{doc: doc, router: router, errs: errSchema.Value}, nil
}

// Check validates one response to req: its status must be documented for
// the operation, and its headers and body must match what the contract
// says for that status. A request to a path or method the contract does not
// list must have been answered 404 or 405 with the Error body.
func (v *Validator) Check(req *http.Request, status int, header http.Header, body []byte) error {
	r := req.Clone(context.Background())
	if p := r.URL.Path; p == LegacyPrefix || strings.HasPrefix(p, LegacyPrefix+"/") {
		u := *r.URL
		u.Path, u.RawPath = "/api/v1"+p, ""
		r.URL = &u
	}
	// gorilla/mux matches the escaped path; keep it consistent.
	r.URL.RawPath = ""
	r.RequestURI = (&url.URL{Path: r.URL.Path, RawQuery: r.URL.RawQuery}).RequestURI()

	route, params, err := v.router.FindRoute(r)
	if err != nil && len(r.URL.Path) > 1 && strings.HasSuffix(r.URL.Path, "/") && status != http.StatusNotFound {
		// Some routes also answer with a trailing slash, as the same
		// operation.
		r.URL.Path = strings.TrimSuffix(r.URL.Path, "/")
		r.RequestURI = (&url.URL{Path: r.URL.Path, RawQuery: r.URL.RawQuery}).RequestURI()
		route, params, err = v.router.FindRoute(r)
	}
	if err != nil {
		if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			return fmt.Errorf("%s %s is not in the contract, but was answered %d", req.Method, req.URL.Path, status)
		}
		return v.checkErrorBody(header, body)
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: r, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		},
		Status: status,
		Header: header,
		Body:   io.NopCloser(bytes.NewReader(body)),
		Options: &openapi3filter.Options{
			IncludeResponseStatus: true,
			MultiError:            true,
		},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		return fmt.Errorf("%s %s answered %d, which the contract does not describe: %w\nbody: %s",
			req.Method, req.URL.Path, status, err, truncate(body))
	}
	// kin-openapi accepts any body for a status documented without one.
	if resp := route.Operation.Responses.Status(status); resp != nil && len(resp.Value.Content) == 0 && len(bytes.TrimSpace(body)) > 0 {
		return fmt.Errorf("%s %s answered %d with a body, but the contract gives that status none: %s",
			req.Method, req.URL.Path, status, truncate(body))
	}
	return nil
}

func (v *Validator) checkErrorBody(header http.Header, body []byte) error {
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return fmt.Errorf("an unknown route was answered with Content-Type %q, not the Error body", ct)
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("an unknown route was answered with a body that is not JSON: %w", err)
	}
	if err := v.errs.VisitJSON(doc, openapi3.MultiErrors()); err != nil {
		return fmt.Errorf("an unknown route was answered without the Error body: %w", err)
	}
	return nil
}

// ValidateSchema checks a JSON value against one of the contract's named
// component schemas, closed as for responses.
func (v *Validator) ValidateSchema(name string, value any) error {
	s := v.doc.Components.Schemas[name]
	if s == nil {
		return fmt.Errorf("contract: no schema named %q", name)
	}
	return s.Value.VisitJSON(value, openapi3.MultiErrors())
}

// closeObjects marks every object schema that lists properties, and does
// not say whether others are allowed, as allowing none.
func closeObjects(doc *openapi3.T) {
	seen := map[*openapi3.Schema]bool{}
	var walk func(ref *openapi3.SchemaRef)
	walk = func(ref *openapi3.SchemaRef) {
		if ref == nil || ref.Value == nil || seen[ref.Value] {
			return
		}
		s := ref.Value
		seen[s] = true
		if len(s.Properties) > 0 && s.AdditionalProperties.Has == nil && s.AdditionalProperties.Schema == nil {
			s.AdditionalProperties.Has = openapi3.Ptr(false)
		}
		for _, p := range s.Properties {
			walk(p)
		}
		walk(s.Items)
		walk(s.AdditionalProperties.Schema)
		for _, list := range []openapi3.SchemaRefs{s.AllOf, s.AnyOf, s.OneOf} {
			for _, sub := range list {
				walk(sub)
			}
		}
	}
	for _, s := range doc.Components.Schemas {
		walk(s)
	}
	for _, r := range doc.Components.Responses {
		for _, m := range r.Value.Content {
			walk(m.Schema)
		}
	}
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			for _, r := range op.Responses.Map() {
				for _, m := range r.Value.Content {
					walk(m.Schema)
				}
			}
		}
	}
}

func truncate(b []byte) string {
	const max = 2000
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
