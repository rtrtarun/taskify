package api

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/rtrtarun/taskify/contract/contracttest"
	"github.com/rtrtarun/taskify/internal/observability"
)

// legacyPrefix is where the original Taskify's task routes are still served,
// as aliases of /api/v1/tasks. The contract documents them once, under
// /api/v1 (its description says so).
const legacyPrefix = "/tasks"

// servedRoutes lists every route the router registers, with every optional
// part configured, as "METHOD /path", sorted. Legacy aliases are returned
// separately, already mapped to the route they alias.
func servedRoutes(t *testing.T) (routes, legacy []string) {
	t.Helper()
	e := newEnv(t)
	d := e.deps
	d.Metrics = observability.NewMetrics(nil)
	mux, err := newMux(d)
	if err != nil {
		t.Fatal(err)
	}
	err = chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if len(route) > 1 {
			route = strings.TrimSuffix(route, "/")
		}
		if route == legacyPrefix || strings.HasPrefix(route, legacyPrefix+"/") {
			legacy = append(legacy, method+" /api/v1"+route)
			return nil
		}
		routes = append(routes, method+" "+route)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(routes)
	slices.Sort(legacy)
	return slices.Compact(routes), slices.Compact(legacy)
}

// TestRoutesMatchContract is M12-01's check: contract/openapi.yaml documents
// exactly the routes the router serves, no more and no fewer.
func TestRoutesMatchContract(t *testing.T) {
	doc, err := contracttest.Doc()
	if err != nil {
		t.Fatal(err)
	}
	documented := contracttest.Operations(doc)
	served, legacy := servedRoutes(t)

	for _, r := range served {
		if !slices.Contains(documented, r) {
			t.Errorf("served but not documented in contract/openapi.yaml: %s", r)
		}
	}
	for _, r := range documented {
		if !slices.Contains(served, r) {
			t.Errorf("documented in contract/openapi.yaml but not served: %s", r)
		}
	}
	// Every legacy alias stands for a documented task route.
	for _, r := range legacy {
		if !slices.Contains(documented, r) {
			t.Errorf("legacy alias of an undocumented route: %s", strings.Replace(r, "/api/v1", "", 1))
		}
	}
	if len(served) < 20 || len(legacy) < 10 {
		t.Errorf("walked %d routes and %d legacy aliases; the walk is missing some", len(served), len(legacy))
	}
}

var (
	validatorOnce sync.Once
	validator     *contracttest.Validator
	validatorErr  error
)

// contractValidator returns the response validator, loaded once.
func contractValidator(t *testing.T) *contracttest.Validator {
	t.Helper()
	validatorOnce.Do(func() { validator, validatorErr = contracttest.NewValidator() })
	if validatorErr != nil {
		t.Fatal(validatorErr)
	}
	return validator
}
