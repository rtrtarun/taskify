package contract

import (
	"regexp"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestAPIVersionIsTheDocumentsInfoVersion(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	if got := APIVersion(); got == "" || got != doc.Info.Version {
		t.Errorf("APIVersion() = %q, info.version = %q", got, doc.Info.Version)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(APIVersion()) {
		t.Errorf("APIVersion() = %q is not major.minor.patch", APIVersion())
	}
}

func TestInfoVersionReader(t *testing.T) {
	for name, tc := range map[string]struct{ doc, want string }{
		"plain":       {"openapi: 3.0.3\ninfo:\n  title: T\n  version: 2.4.1\n  description: x\npaths: {}\n", "2.4.1"},
		"quoted":      {"info:\n  version: \"1.0.0\"\n", "1.0.0"},
		"no version":  {"info:\n  title: T\npaths:\n  version: 9.9.9\n", ""},
		"no info":     {"openapi: 3.0.3\n", ""},
		"nested only": {"info:\n  contact:\n    version: 1\n", ""},
	} {
		if got := infoVersion([]byte(tc.doc)); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
