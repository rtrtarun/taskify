package conformance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"
	"time"
)

// These tests need no target: they check the fixtures, the rules' citations,
// and the runner's own logic.

func loadAll(t *testing.T) []*Fixture {
	t.Helper()
	fixtures, err := LoadFixtures(fixturesDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatalf("loaded %d fixtures", len(fixtures))
	}
	return fixtures
}

// TestFixturesAreWellFormed: every fixture parses strictly, names rules
// that contract/RULES.md defines, and covers each group of section 9.3.
func TestFixturesAreWellFormed(t *testing.T) {
	rules, err := os.ReadFile("../../contract/RULES.md")
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]int{}
	for _, f := range loadAll(t) {
		groups[strings.Split(f.Name, "/")[0]]++
		if len(f.Rules) == 0 {
			t.Errorf("%s names no rule", f.Name)
		}
		for _, r := range f.Rules {
			if !strings.Contains(string(rules), "| "+r+" ") && !strings.Contains(string(rules), r+", ") {
				t.Errorf("%s names %s, which contract/RULES.md does not define", f.Name, r)
			}
		}
	}
	for _, g := range []string{"tasks", "transitions", "events", "delivery", "webhooks", "auth", "agents", "hitl"} {
		if groups[g] == 0 {
			t.Errorf("no fixture in group %s", g)
		}
	}
}

// TestRunnerImportsNoServerPackage: the runner is a black box. Neither it
// nor anything it imports may use the server's packages (section 9.3).
func TestRunnerImportsNoServerPackage(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go command is not on PATH")
	}
	out, err := exec.Command(goBin, "list", "-deps", "-test", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasPrefix(pkg, "github.com/rtrtarun/taskify/internal/") || strings.HasPrefix(pkg, "github.com/rtrtarun/taskify/cmd/") {
			t.Errorf("the runner depends on server package %s", pkg)
		}
	}
	if !strings.Contains(string(out), "github.com/rtrtarun/taskify/contract/contracttest") {
		t.Error("go list did not list the runner's own imports; the check proves nothing")
	}
}

func TestMatch(t *testing.T) {
	doc := map[string]any{
		"id": "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", "at": "2026-11-22T09:00:00.5Z", "n": 2.0, "ok": true, "none": nil,
		"list": []any{1.0, map[string]any{"a": "x", "b": "y"}}, "name": "bob",
	}
	for name, pattern := range map[string]string{
		"subset":   `{"name": "bob", "n": 2}`,
		"kinds":    `{"id": "$uuid", "at": "$timestamp", "name": "$nonempty", "ok": "$any", "missing": "$absent"}`,
		"null":     `{"none": null}`,
		"list":     `{"list": [1, {"a": "x"}]}`,
		"length":   `{"list": {"$length": 2}}`,
		"contains": `{"list": {"$contains": [{"b": "y"}]}}`,
		"one of":   `{"name": {"$one_of": ["alice", "bob"]}}`,
	} {
		var p any
		if err := json.Unmarshal([]byte(pattern), &p); err != nil {
			t.Fatal(err)
		}
		if err := match(p, doc, ""); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, pattern := range map[string]string{
		"value":       `{"name": "alice"}`,
		"missing key": `{"nope": 1}`,
		"absent":      `{"name": "$absent"}`,
		"uuid":        `{"name": "$uuid"}`,
		"timestamp":   `{"name": "$timestamp"}`,
		"nonempty":    `{"none": "$nonempty"}`,
		"number":      `{"n": 3}`,
		"bool":        `{"ok": false}`,
		"null":        `{"name": null}`,
		"length":      `{"list": {"$length": 3}}`,
		"list length": `{"list": [1]}`,
		"contains":    `{"list": {"$contains": [{"b": "z"}]}}`,
		"one of":      `{"name": {"$one_of": ["alice"]}}`,
		"not object":  `{"name": {"x": 1}}`,
		"not list":    `{"name": [1]}`,
	} {
		var p any
		_ = json.Unmarshal([]byte(pattern), &p)
		if err := match(p, doc, ""); err == nil {
			t.Errorf("%s: matched", name)
		}
	}
	if err := matchSet([]any{map[string]any{"a": 1.0}}, []any{map[string]any{"a": 1.0}, map[string]any{"a": 2.0}}, true); err == nil {
		t.Error("an exact set ignored an extra item")
	}
	if err := matchSet([]any{map[string]any{"a": 1.0}}, []any{map[string]any{"a": 2.0}, map[string]any{"a": 1.0}}, false); err != nil {
		t.Errorf("an inclusive set: %v", err)
	}
}

func TestTemplates(t *testing.T) {
	v := newVars("conf-1", "conf-1-other", time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	v["version"] = json.Number("3")
	if got, _ := v.expand("{{version}}"); got != json.Number("3") {
		t.Errorf("a whole placeholder keeps its type: %v", got)
	}
	if got, _ := v.expandString("taskify:{{tenant}}:v{{version}}"); got != "taskify:conf-1:v3" {
		t.Errorf("embedded = %q", got)
	}
	if got, _ := v.expandString("{{in_2h}} {{in_2h_unix}} {{now_date}}"); got != "2026-11-22T11:00:00Z 1795345200 2026-11-22" {
		t.Errorf("times = %q", got)
	}
	if _, err := v.expand("{{nope}}"); err == nil {
		t.Error("an unknown variable expanded")
	}
	if _, err := v.expandString("a {{nope}} b"); err == nil {
		t.Error("an unknown embedded variable expanded")
	}
	doc, err := v.expandJSON(json.RawMessage(`{"a": ["{{tenant}}", {"b": "{{version}}"}], "c": 1}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := show(doc); got != `{"a":["conf-1",{"b":3}],"c":1}` {
		t.Errorf("expandJSON = %s", got)
	}
	if got, ok := lookup(map[string]any{"a": []any{map[string]any{"b": "c"}}}, "a.0.b"); !ok || got != "c" {
		t.Errorf("lookup = %v, %v", got, ok)
	}
	for _, p := range []string{"a.1.b", "a.x", "z", "a.0.b.c"} {
		if _, ok := lookup(map[string]any{"a": []any{map[string]any{"b": "c"}}}, p); ok {
			t.Errorf("lookup(%s) found something", p)
		}
	}
	if text(2.5) != "2.5" || text(nil) != "null" || text(true) != "true" {
		t.Error("text")
	}
}

func TestIdentities(t *testing.T) {
	for raw, want := range map[string]identity{
		``:                                   {user: "usr_bob", role: "member"},
		`"admin"`:                            {user: "usr_admin", role: "admin"},
		`"other_tenant"`:                     {user: "usr_eve", role: "admin", tenant: "{{other_tenant}}"},
		`"headers"`:                          {kind: "headers", user: "usr_bob", role: "member"},
		`{"user": "usr_carol"}`:              {user: "usr_carol", role: "member"},
		`{"user": "u", "tenant": "default"}`: {user: "u", role: "member", tenant: "default"},
	} {
		got, err := parseIdentity(json.RawMessage(raw))
		if err != nil || got != want {
			t.Errorf("%s = %+v, %v", raw, got, err)
		}
	}
	for _, bad := range []string{`"root"`, `{"role": "admin"}`, `{"user": "u", "extra": 1}`, `5`} {
		if _, err := parseIdentity(json.RawMessage(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestLoadRefusesBadFixtures(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":    `{"description": "d", "steps": [{"request": {"method": "GET", "path": "/x"}, "expect": {"status": 200}}], "extra": 1}`,
		"no description":   `{"steps": [{"request": {"method": "GET", "path": "/x"}, "expect": {"status": 200}}]}`,
		"no steps":         `{"description": "d", "steps": []}`,
		"no status":        `{"description": "d", "steps": [{"request": {"method": "GET", "path": "/x"}, "expect": {}}]}`,
		"both":             `{"description": "d", "steps": [{"request": {"method": "GET", "path": "/x"}, "webhook": {}, "expect": {"status": 200}}]}`,
		"relative path":    `{"description": "d", "steps": [{"request": {"method": "GET", "path": "x"}, "expect": {"status": 200}}]}`,
		"unknown identity": `{"description": "d", "steps": [{"as": "root", "request": {"method": "GET", "path": "/x"}, "expect": {"status": 200}}]}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(path.Join(dir, "f.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFixtures(dir); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}
