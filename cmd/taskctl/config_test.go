package main

import (
	"os"
	"strings"
	"testing"
)

func TestPrecedenceFlagThenEnvironmentThenProfile(t *testing.T) {
	c := newCLI(t)
	// Profile values.
	for _, kv := range [][]string{{"server", "http://profile"}, {"workspace", "ws_profile"}} {
		if code, _, e := c.run("config", "set", kv[0], kv[1]); code != 0 {
			t.Fatalf("config set: %d %s", code, e)
		}
	}
	resolve := func(args ...string) settings {
		t.Helper()
		var o, e strings.Builder
		a := newApp(strings.NewReader(""), &o, &e, func(k string) string { return c.env[k] })
		if err := a.newRoot().ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		cfg, err := a.loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		s, err := a.resolve(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	if s := resolve(); s.server != "http://profile" || s.sources["server"] != "profile" {
		t.Errorf("profile only: %+v", s)
	}
	c.env["TASKIFY_SERVER"] = "http://env"
	if s := resolve(); s.server != "http://env" || s.sources["server"] != "environment" {
		t.Errorf("environment over profile: %+v", s)
	}
	if s := resolve("--server", "http://flag"); s.server != "http://flag" || s.sources["server"] != "flag" {
		t.Errorf("flag over environment: %+v", s)
	}

	// The profile itself: current_profile, then environment, then flag.
	if s := resolve(); s.profile != "default" {
		t.Errorf("default profile: %+v", s)
	}
	c.env["TASKIFY_PROFILE"] = "ci"
	if s := resolve(); s.profile != "ci" || s.server != "http://env" || s.workspace != "" {
		t.Errorf("TASKIFY_PROFILE: %+v", s) // workspace is the default profile's, not ci's
	}
	if s := resolve("--profile", "dev"); s.profile != "dev" {
		t.Errorf("--profile: %+v", s)
	}
}

func TestProfilesAreSeparateAndSwitchable(t *testing.T) {
	c := newCLI(t)
	c.run("config", "set", "server", "http://one")
	c.run("--profile", "other", "config", "set", "server", "http://two")
	if _, out, _ := c.run("config", "view"); !strings.Contains(out, "http://one") {
		t.Errorf("view: %s", out)
	}
	if code, _, e := c.run("config", "use-profile", "other"); code != 0 {
		t.Fatalf("use-profile: %d %s", code, e)
	}
	if _, out, _ := c.run("config", "view"); !strings.Contains(out, "http://two") || !strings.Contains(out, "[default other]") {
		t.Errorf("view after switch: %s", out)
	}
}

func TestConfigRefusesWhatItShouldNot(t *testing.T) {
	c := newCLI(t)
	for name, args := range map[string][]string{
		"token is not a setting": {"config", "set", "token", "abc"},
		"unknown key":            {"config", "set", "colour", "red"},
		"bad profile name":       {"config", "use-profile", "Bad.Name"},
		"missing value":          {"config", "set", "server"},
		"unknown flag":           {"--nope"},
		"unknown command":        {"frobnicate"},
		"unknown subcommand":     {"task", "frobnicate"},
		"unknown admin command":  {"admin", "outbox", "frobnicate"},
	} {
		if code, _, _ := c.run(args...); code != exitUsage {
			t.Errorf("%s: exit %d, want %d", name, code, exitUsage)
		}
	}
}

func TestConfigFileHoldsNoCredentials(t *testing.T) {
	c := newCLI(t)
	const token = "tok-very-secret-1234567890"
	url := serve(t, 200, meBody, nil)
	c.stdin = token + "\n"
	if code, _, e := c.run("login", "--server", url); code != 0 {
		t.Fatalf("login: %d %s", code, e)
	}
	raw, err := os.ReadFile(c.env["TASKIFY_CONFIG"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Error("the token is in config.yaml")
	}
	if !strings.Contains(string(raw), "tenant_acme") {
		t.Errorf("config.yaml should record the tenant: %s", raw)
	}
}
