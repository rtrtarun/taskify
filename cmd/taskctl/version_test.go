package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rtrtarun/taskify/pkg/client"
)

// The newer-server notice: once per run, on stderr, never an error.
func TestNewerServerNotice(t *testing.T) {
	known := client.GeneratedAPIVersion()
	for name, tc := range map[string]struct {
		header string
		warns  bool
	}{
		"newer major":   {"99.0.0", true},
		"newer minor":   {"1.99.0", known != "" && client.CompareVersions("1.99.0", known) > 0},
		"same":          {known, false},
		"older":         {"0.0.1", false},
		"no header":     {"", false},
		"unreadable":    {"banana", false},
		"newer pre":     {"99.0.0-rc.1", true},
		"pre of newest": {known + "-rc.1", false}, // a pre-release of the known version is older than it
	} {
		var stderr bytes.Buffer
		a := newApp(strings.NewReader(""), &bytes.Buffer{}, &stderr, func(string) string { return "" })
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tc.header != "" {
				w.Header().Set(client.HeaderAPIVersion, tc.header)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(meBody))
		}))
		hc := &http.Client{Transport: &versionTransport{app: a}}
		for i := 0; i < 3; i++ { // three requests in one run
			resp, err := hc.Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		}
		srv.Close()
		n := strings.Count(stderr.String(), "newer than")
		switch {
		case tc.warns && n != 1:
			t.Errorf("%s: %d notices, want exactly 1: %q", name, n, stderr.String())
		case !tc.warns && n != 0:
			t.Errorf("%s: unexpected notice %q", name, stderr.String())
		}
		if tc.warns && !strings.Contains(stderr.String(), tc.header) {
			t.Errorf("%s: notice does not name the server's version: %q", name, stderr.String())
		}
	}
}

// Against a real server of the same version, a command prints no notice and
// exits 0.
func TestNoNoticeAgainstTheSameVersion(t *testing.T) {
	c, _ := local(t)
	code, _, errb := c.run("workspace", "list")
	if code != 0 || strings.Contains(errb, "newer than") {
		t.Errorf("exit %d, stderr %q", code, errb)
	}
}

func TestNewerServerIsNotAnError(t *testing.T) {
	c := newCLI(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(client.HeaderAPIVersion, "99.1.0")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(meBody))
	}))
	t.Cleanup(srv.Close)
	c.stdin = "tok-abcdefghij\n"
	code, _, errb := c.run("login", "--server", srv.URL)
	if code != 0 || strings.Count(errb, "newer than") != 1 {
		t.Errorf("exit %d, stderr %q", code, errb)
	}
}
