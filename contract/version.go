package contract

import (
	"bufio"
	"bytes"
	"strings"
	"sync"
)

var (
	versionOnce sync.Once
	apiVersion  string
)

// APIVersion is the version of the API contract: `info.version` of the
// embedded openapi.yaml, the one place the version is written down. The
// server sends it in X-Taskify-API-Version on every response, and clients
// compare it with the version they were generated from (pkg/client).
//
// It is "" only if the document has no info.version, which a test rules out.
func APIVersion() string {
	versionOnce.Do(func() { apiVersion = infoVersion(OpenAPI) })
	return apiVersion
}

// infoVersion reads `version:` from the top-level `info:` block without a
// YAML parser, so the package keeps depending on nothing outside the
// standard library. The block is the lines indented under `info:`.
func infoVersion(doc []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(doc))
	in := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "info:":
			in = true
		case in && line != "" && line[0] != ' ' && line[0] != '#':
			return "" // the next top-level key: info had no version
		case in && strings.HasPrefix(line, "  version:"):
			v := strings.TrimSpace(strings.TrimPrefix(line, "  version:"))
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
