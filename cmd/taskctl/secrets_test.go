package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// The exit criterion: no credential is printed, including with --verbose,
// on success or on any failure, even when the server echoes the token back.
func TestTokenNeverAppearsInOutput(t *testing.T) {
	tokens := map[string]string{
		"jwt-shaped": "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c3JfYW5uIn0.c2lnbmF0dXJlMTIzNDU2",
		"opaque":     "opaque-token-0123456789abcdef",
	}
	responses := map[string]struct {
		status int
		body   string
	}{
		"success":        {200, meBody},
		"echoing server": {401, `{"error":{"code":"unauthorized","message":"bad token TOKEN, header Bearer TOKEN"}}`},
		"server error":   {500, `{"error":{"code":"internal_error","message":"boom TOKEN"}}`},
	}
	for tname, token := range tokens {
		for rname, rs := range responses {
			t.Run(tname+"/"+rname, func(t *testing.T) {
				c := newCLI(t)
				c.stdin = token + "\n"
				body := strings.ReplaceAll(rs.body, "TOKEN", token)
				url := serve(t, rs.status, body, func(r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer "+token {
						t.Error("the server did not receive the token")
					}
				})
				code, out, errb := c.run("--verbose", "login", "--server", url)
				all := out + errb
				if strings.Contains(all, token) {
					t.Fatalf("exit %d: the token is in the output:\n%s", code, all)
				}
				if rs.status == 200 {
					if !strings.Contains(errb, "> GET") || !strings.Contains(errb, "< 200") {
						t.Errorf("--verbose printed no trace: %q", errb)
					}
					// Later commands do not show it either.
					_, view, _ := c.run("--verbose", "--server", url, "config", "view")
					if strings.Contains(view, token) || !strings.Contains(view, "set (keychain)") {
						t.Errorf("config view: %s", view)
					}
				}
			})
		}
	}
}

func TestEnvironmentTokenIsUsedAndHidden(t *testing.T) {
	c := newCLI(t)
	const token = "env-token-0123456789"
	var got string
	url := serve(t, 200, meBody, func(r *http.Request) { got = r.Header.Get("Authorization") })
	c.env["TASKIFY_TOKEN"] = token
	c.run("config", "set", "server", url)
	_, view, _ := c.run("config", "view")
	if strings.Contains(view, token) || !strings.Contains(view, "set (environment)") {
		t.Errorf("config view: %s", view)
	}
	// An API command (added in M13-03) sends it; here the client factory does.
	var out, errb strings.Builder
	a := newApp(strings.NewReader(""), &out, &errb, func(k string) string { return c.env[k] })
	a.keychain = c.keychain
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	cl, err := a.newClient(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.GetMeWithResponse(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer "+token {
		t.Errorf("Authorization = %q", got)
	}
}

func TestLoginRefusesARejectedTokenAndStoresNothing(t *testing.T) {
	c := newCLI(t)
	c.stdin = "tok-rejected-0123\n"
	url := serve(t, 401, `{"error":{"code":"unauthorized","message":"invalid token"}}`, nil)
	if code, _, _ := c.run("login", "--server", url); code != exitAuth {
		t.Fatalf("exit %d", code)
	}
	if len(c.keychain.m) != 0 {
		t.Error("a rejected token was stored")
	}
	if _, err := os.Stat(c.env["TASKIFY_CONFIG"]); err == nil {
		t.Error("a config file was written for a failed login")
	}
}

func TestLoginFallsBackToTheFile(t *testing.T) {
	c := newCLI(t)
	c.keychain.fail = true
	c.stdin = "tok-file-0123456789\n"
	url := serve(t, 200, meBody, nil)
	code, out, _ := c.run("login", "--server", url)
	if code != 0 || !strings.Contains(out, "stored in the file") || strings.Contains(out, "tok-file") {
		t.Fatalf("exit %d: %s", code, out)
	}
	_, view, _ := c.run("config", "view")
	if !strings.Contains(view, "set (file)") {
		t.Errorf("view: %s", view)
	}
}
