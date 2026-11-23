package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKeychain is an in-memory keychain; fail makes every call error, like a
// machine with no keychain.
type fakeKeychain struct {
	m    map[string]string
	fail bool
}

func (k *fakeKeychain) Get(s, u string) (string, error) {
	if k.fail {
		return "", errors.New("no keychain")
	}
	if v, ok := k.m[s+"/"+u]; ok {
		return v, nil
	}
	return "", errors.New("not found")
}

func (k *fakeKeychain) Set(s, u, p string) error {
	if k.fail {
		return errors.New("no keychain")
	}
	k.m[s+"/"+u] = p
	return nil
}

func (k *fakeKeychain) Delete(s, u string) error {
	delete(k.m, s+"/"+u)
	return nil
}

// cli runs taskctl in a temporary configuration directory.
type cli struct {
	t        *testing.T
	dir      string
	env      map[string]string
	keychain *fakeKeychain
	stdin    string
}

func newCLI(t *testing.T) *cli {
	dir := t.TempDir()
	return &cli{t: t, dir: dir, keychain: &fakeKeychain{m: map[string]string{}},
		env: map[string]string{"TASKIFY_CONFIG": filepath.Join(dir, "config.yaml")}}
}

func (c *cli) app(stdout, stderr *bytes.Buffer) *app {
	a := newApp(strings.NewReader(c.stdin), stdout, stderr, func(k string) string { return c.env[k] })
	a.keychain = c.keychain
	return a
}

// run returns the exit code, standard output, and standard error.
func (c *cli) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := c.app(&out, &errb).run(args)
	return code, out.String(), errb.String()
}

// serve starts a stub server answering every request with status and body.
func serve(t *testing.T, status int, body string, check func(*http.Request)) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const meBody = `{"tenant_id":"tenant_acme","user_id":"usr_ann","role":"member"}`
