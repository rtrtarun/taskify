package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var statuses = []string{"todo", "in_progress", "blocked", "completed", "archived"}

// TestTransitionMatrixHasEveryCell is M12-07's check: one fixture per cell
// of the 5 by 5 status matrix, whose last change asks for that cell.
func TestTransitionMatrixHasEveryCell(t *testing.T) {
	byName := map[string]*Fixture{}
	for _, f := range loadAll(t) {
		byName[f.Name] = f
	}
	for _, from := range statuses {
		for _, to := range statuses {
			name := "transitions/" + from + "-to-" + to
			f := byName[name]
			if f == nil {
				t.Errorf("no fixture for %s -> %s (%s.json)", from, to, name)
				continue
			}
			var last *Step
			for i := range f.Steps {
				if s := &f.Steps[i]; s.Request != nil && s.Request.Method == "PATCH" {
					last = s
				}
			}
			var body struct {
				Status string `json:"status"`
			}
			if last == nil || json.Unmarshal(last.Request.Body, &body) != nil || body.Status != to {
				t.Errorf("%s does not end by asking for %s", name, to)
			}
		}
	}
}

var (
	ruleRow  = regexp.MustCompile(`^\| (R\d+\.\d+) `)
	citation = regexp.MustCompile("`([a-z]+/[a-z0-9*_-]+\\.json)`")
)

// TestRulesCiteFixtures is M12-03's check: every numbered rule in
// contract/RULES.md cites a fixture, and every cited fixture exists.
func TestRulesCiteFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../contract/RULES.md")
	if err != nil {
		t.Fatal(err)
	}
	numbered := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if m := ruleRow.FindStringSubmatch(line); m != nil {
			numbered++
			if !citation.MatchString(line) {
				t.Errorf("%s cites no fixture", m[1])
			}
		}
		for _, c := range citation.FindAllStringSubmatch(line, -1) {
			matches, err := filepath.Glob(filepath.Join(fixturesDir, filepath.FromSlash(c[1])))
			if err != nil || len(matches) == 0 {
				t.Errorf("contract/RULES.md cites %s, which does not exist", c[1])
			}
		}
	}
	if numbered < 30 {
		t.Errorf("found %d numbered rules", numbered)
	}
}
