package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const secret = "dev-secret"

func TestDevTokenTenants(t *testing.T) {
	now := time.Now()
	for _, tenant := range []string{"tenant_dev", "Acme.Corp-1", "a", "Default", strings.Repeat("t", 64)} {
		var out, errOut bytes.Buffer
		if code := run([]string{"-tenant", tenant, "-user", "usr_bob", "-role", "member"}, secret, now, &out, &errOut); code != 0 {
			t.Errorf("tenant %q: exit %d: %s", tenant, code, errOut.String())
			continue
		}
		var c jwt.MapClaims
		_, err := jwt.ParseWithClaims(strings.TrimSpace(out.String()), &c, func(*jwt.Token) (any, error) { return []byte(secret), nil })
		if err != nil || c["tenant_id"] != tenant || c["sub"] != "usr_bob" || c["role"] != "member" {
			t.Errorf("tenant %q: claims %v, err %v", tenant, c, err)
		}
	}

	// The default tenant still works without flags.
	var out bytes.Buffer
	if code := run(nil, secret, now, &out, &bytes.Buffer{}); code != 0 || out.Len() == 0 {
		t.Fatalf("no flags: exit %d", code)
	}
	var c jwt.MapClaims
	if _, err := jwt.ParseWithClaims(strings.TrimSpace(out.String()), &c, func(*jwt.Token) (any, error) { return []byte(secret), nil }); err != nil || c["tenant_id"] != "tenant_dev" {
		t.Errorf("default claims %v, err %v", c, err)
	}

	for _, tenant := range []string{"default", "", " tenant_dev", "tenant_dev ", "-acme", "_acme", "acme corp", "acme/1", "ténant", strings.Repeat("t", 65)} {
		var out, errOut bytes.Buffer
		if code := run([]string{"-tenant", tenant}, secret, now, &out, &errOut); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "-tenant must match") {
			t.Errorf("tenant %q: exit %d, stdout %q, stderr %q", tenant, code, out.String(), errOut.String())
		}
	}
}

func TestDevTokenOtherFlags(t *testing.T) {
	for name, tc := range map[string]struct {
		args   []string
		secret string
		code   int
	}{
		"no secret":    {nil, "", 1},
		"bad role":     {[]string{"-role", "owner"}, secret, 2},
		"empty user":   {[]string{"-user", ""}, secret, 2},
		"unknown flag": {[]string{"-nope"}, secret, 2},
	} {
		var out bytes.Buffer
		if code := run(tc.args, tc.secret, time.Now(), &out, &bytes.Buffer{}); code != tc.code || out.Len() != 0 {
			t.Errorf("%s: exit %d, want %d; stdout %q", name, code, tc.code, out.String())
		}
	}
}

// TestDevTokenUsers is AP-11: -user must be a user ID the server accepts, 1
// to 128 printable ASCII characters, with no spaces around it.
func TestDevTokenUsers(t *testing.T) {
	now := time.Now()
	for _, user := range []string{"usr_bob", "a", "alice@example.com", "Bob Smith", strings.Repeat("u", 128)} {
		var out, errOut bytes.Buffer
		if code := run([]string{"-user", user}, secret, now, &out, &errOut); code != 0 {
			t.Errorf("user %q: exit %d: %s", user, code, errOut.String())
			continue
		}
		var c jwt.MapClaims
		_, err := jwt.ParseWithClaims(strings.TrimSpace(out.String()), &c, func(*jwt.Token) (any, error) { return []byte(secret), nil })
		if err != nil || c["sub"] != user {
			t.Errorf("user %q: claims %v, err %v", user, c, err)
		}
	}
	for _, user := range []string{"", " usr_bob", "usr_bob ", "josé", "usr\tbob", "usr\nbob", strings.Repeat("u", 129)} {
		var out, errOut bytes.Buffer
		if code := run([]string{"-user", user}, secret, now, &out, &errOut); code != 2 || out.Len() != 0 ||
			!strings.Contains(errOut.String(), "-user must be 1 to 128 printable ASCII") {
			t.Errorf("user %q: exit %d, stdout %q, stderr %q", user, code, out.String(), errOut.String())
		}
	}
}
