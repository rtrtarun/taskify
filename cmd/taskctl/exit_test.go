package main

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// login calls GET /me, so a stub answering it exercises every way a call can
// end.
func TestExitCodes(t *testing.T) {
	errBody := func(code string) string {
		return `{"error":{"code":"` + code + `","message":"nope","details":{"current_version":7}}}`
	}
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"success", 200, meBody, exitOK},
		{"unauthenticated", 401, errBody("unauthorized"), exitAuth},
		{"forbidden", 403, errBody("forbidden"), exitAuth},
		{"not found", 404, errBody("not_found"), exitNotFound},
		{"version conflict", 409, errBody("version_conflict"), exitConflict},
		{"stale proposal", 409, errBody("stale_version"), exitConflict},
		{"other conflict", 409, errBody("already_decided"), exitUnexpected},
		{"validation", 422, errBody("validation_failed"), exitUsage},
		{"bad request", 400, errBody("invalid_request"), exitUsage},
		{"server error", 500, errBody("internal_error"), exitUnexpected},
		{"not json", 502, "<html>", exitUnexpected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLI(t)
			c.stdin = "tok-abcdefghij\n"
			url := serve(t, tc.status, tc.body, nil)
			code, _, stderr := c.run("login", "--server", url)
			if code != tc.want {
				t.Errorf("exit %d, want %d (stderr %q)", code, tc.want, stderr)
			}
			if tc.want == exitConflict && !strings.Contains(stderr, "version 7") {
				t.Errorf("a conflict should report the current version: %q", stderr)
			}
		})
	}

	t.Run("server unreachable", func(t *testing.T) {
		c := newCLI(t)
		c.stdin = "tok-abcdefghij\n"
		url := serve(t, 200, meBody, nil)
		// Close the listener's server by pointing at a port nothing uses.
		dead := strings.Replace(url, url[strings.LastIndex(url, ":")+1:], "1", 1)
		if code, _, _ := c.run("login", "--server", dead); code != exitUnreachable {
			t.Errorf("exit %d, want %d", code, exitUnreachable)
		}
	})
	t.Run("usage", func(t *testing.T) {
		c := newCLI(t)
		for _, args := range [][]string{{"login", "extra"}, {"--bogus"}, {"config", "set"}} {
			if code, _, _ := c.run(args...); code != exitUsage {
				t.Errorf("%v: exit %d", args, code)
			}
		}
		c.stdin = ""
		if code, _, _ := c.run("login", "--server", "http://x"); code != exitUsage {
			t.Errorf("login without a token: exit %d", code)
		}
		if code, _, _ := c.run("login"); code != exitUsage {
			t.Errorf("login without a server: exit %d", code)
		}
	})
	t.Run("no credentials", func(t *testing.T) {
		c := newCLI(t)
		c.run("config", "set", "server", "http://127.0.0.1:1")
		var out, errb bytes.Buffer
		a := c.app(&out, &errb)
		s, err := a.session()
		if err != nil {
			t.Fatal(err)
		}
		_, err = a.token(s)
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != exitAuth || !strings.Contains(ee.msg, "taskctl login") {
			t.Errorf("token() = %v", err)
		}
	})
}

func TestStatusExit(t *testing.T) {
	if statusExit(http.StatusTeapot, "") != exitUnexpected {
		t.Error("an unknown status is unexpected")
	}
}
