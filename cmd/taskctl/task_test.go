package main

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/rtrtarun/taskify/tests/localserver"
)

// local starts a server and points the CLI at it as usr_ann, a member.
func local(t *testing.T) (*cli, *localserver.Server) {
	t.Helper()
	srv := localserver.Start(t)
	c := newCLI(t)
	c.env["TASKIFY_SERVER"] = srv.URL
	c.env["TASKIFY_TOKEN"] = srv.Token(t, "usr_ann", "member")
	return c, srv
}

// field reads "key: value" from a task card.
func field(card, key string) string {
	m := regexp.MustCompile(`(?m)^` + key + `:[ ]+(.*)$`).FindStringSubmatch(card)
	if m == nil {
		return ""
	}
	return m[1]
}

func (c *cli) ok(args ...string) string {
	c.t.Helper()
	code, out, errb := c.run(args...)
	if code != 0 {
		c.t.Fatalf("taskctl %v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errb)
	}
	return out
}

var idLine = regexp.MustCompile(`(?m)^id:\s+([0-9a-f-]{36})$`)

// add creates a task and returns its full ID.
func (c *cli) add(title string, flags ...string) string {
	c.t.Helper()
	out := c.ok(append([]string{"task", "add", title, "--workspace", localserver.Workspace}, flags...)...)
	m := idLine.FindStringSubmatch(out)
	if m == nil {
		c.t.Fatalf("no id in: %s", out)
	}
	return m[1]
}

func TestTaskAddShowListAndPrefix(t *testing.T) {
	c, _ := local(t)
	id := c.add("Write the report", "--desc", "Q4 numbers", "--priority", "p2_high", "--label", "finance", "--label", "q4", "--due", "2027-01-31")

	show := c.ok("task", "show", id)
	for _, want := range []string{"Write the report", "p2_high", "finance, q4", "2027-01-31 00:00Z", "Q4 numbers", "version:   1"} {
		if !strings.Contains(show, want) {
			t.Errorf("show lacks %q:\n%s", want, show)
		}
	}
	if field(show, "version") != "1" {
		t.Errorf("version: %q", field(show, "version"))
	}
	// A unique prefix does as well as the full ID.
	if got := c.ok("task", "show", id[:13]); !strings.Contains(got, id) {
		t.Errorf("prefix show: %s", got)
	}
	if list := c.ok("task", "list"); !strings.Contains(list, id[:8]) || !strings.Contains(list, "Write the report") {
		t.Errorf("list: %s", list)
	}

	// Workspace from the profile.
	c.ok("config", "set", "workspace", localserver.Workspace)
	if out := c.ok("task", "add", "From the profile"); !strings.Contains(out, "From the profile") {
		t.Error(out)
	}

	// Filters.
	c.ok("task", "update", id, "--assignee", "usr_ann")
	if l := c.ok("task", "list", "--mine"); !strings.Contains(l, "Write the report") || strings.Contains(l, "From the profile") {
		t.Errorf("--mine: %s", l)
	}
	if l := c.ok("task", "list", "--status", "completed"); strings.Contains(l, "Write the report") {
		t.Errorf("--status: %s", l)
	}
	if l := c.ok("task", "list", "--priority", "p2_high"); !strings.Contains(l, "Write the report") || strings.Contains(l, "From the profile") {
		t.Errorf("--priority: %s", l)
	}
	if code, _, _ := c.run("task", "list", "--mine", "--assignee", "x"); code != exitUsage {
		t.Errorf("--mine with --assignee: exit %d", code)
	}
}

func TestTaskPrefixErrors(t *testing.T) {
	c, _ := local(t)
	a := c.add("one")
	b := c.add("two")
	common := 0
	for common < len(a) && a[common] == b[common] {
		common++
	}
	if common > 0 {
		code, _, errb := c.run("task", "show", a[:common])
		if code != exitUsage || !strings.Contains(errb, "more than one task") {
			t.Errorf("ambiguous prefix: exit %d, %q", code, errb)
		}
	}
	for name, tc := range map[string]struct {
		arg  string
		want int
	}{
		"no task has it":  {"ffffffff", exitNotFound},
		"unknown full ID": {"0193a1b2-c3d4-7e5f-8a9b-0123456789ab", exitNotFound},
		"not an ID":       {"hello", exitUsage},
	} {
		if code, _, errb := c.run("task", "show", tc.arg); code != tc.want {
			t.Errorf("%s: exit %d, want %d (%s)", name, code, tc.want, errb)
		}
	}
}

func TestTaskUpdateDoneBlockUnblockArchiveComment(t *testing.T) {
	c, _ := local(t)
	id := c.add("Plan the launch")

	out := c.ok("task", "update", id[:8], "--title", "Plan the big launch", "--priority", "p1_critical", "--assignee", "usr_bob", "--due", "2027-02-01T09:30:00Z")
	for _, want := range []string{"Plan the big launch", "p1_critical", "usr_bob", "2027-02-01 09:30Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("update lacks %q:\n%s", want, out)
		}
	}
	if field(out, "version") != "2" {
		t.Errorf("version after update: %q", field(out, "version"))
	}
	out = c.ok("task", "update", id, "--assignee", "", "--due", "", "--desc", "Details")
	if field(out, "assignee") != "-" || field(out, "due") != "-" || !strings.Contains(out, "Details") {
		t.Errorf("clearing fields: %s", out)
	}
	if code, _, _ := c.run("task", "update", id); code != exitUsage {
		t.Errorf("update with nothing to change: exit %d", code)
	}
	if code, _, _ := c.run("task", "update", id, "--priority", "urgent"); code != exitUsage {
		t.Errorf("invalid priority: exit %d", code)
	}

	out = c.ok("task", "block", id, "--reason", "waiting for legal")
	if field(out, "status") != "blocked" || field(out, "blocked") != "manual" {
		t.Errorf("block: %s", out)
	}
	if show := c.ok("task", "show", id, "--comments"); !strings.Contains(show, "Blocked: waiting for legal") {
		t.Errorf("the reason is not a comment:\n%s", show)
	}
	if out := c.ok("task", "unblock", id); field(out, "status") != "in_progress" {
		t.Errorf("unblock: %s", out)
	}

	if out := c.ok("task", "comment", id, "Looks good @usr_bob"); !strings.Contains(out, "added to task") {
		t.Errorf("comment: %s", out)
	}
	if show := c.ok("task", "show", id, "--comments"); !strings.Contains(show, "usr_ann: Looks good @usr_bob") {
		t.Errorf("comments:\n%s", show)
	}

	if out := c.ok("task", "done", id); field(out, "status") != "completed" {
		t.Errorf("done: %s", out)
	}
	if code, _, _ := c.run("task", "unblock", id); code == 0 {
		t.Error("a completed task cannot be unblocked")
	}
	if out := c.ok("task", "archive", id[:8]); field(out, "status") != "archived" {
		t.Errorf("archive: %s", out)
	}
	// An archived task is still found by prefix.
	if out := c.ok("task", "show", id[:8]); !strings.Contains(out, "archived") {
		t.Errorf("show archived: %s", out)
	}
}

// swapTransport lets a test act between the CLI's read and its write.
type swapTransport struct {
	next   http.RoundTripper
	before func(*http.Request)
}

func (s swapTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if s.before != nil {
		s.before(r)
	}
	return s.next.RoundTrip(r)
}

func TestTaskUpdateConflict(t *testing.T) {
	c, srv := local(t)
	id := c.add("Contested")
	other := srv.Token(t, "usr_bob", "member")

	// Another user edits the task after the CLI has read it and before it
	// writes: the CLI's PATCH carries a stale If-Match.
	var patches int
	var out, errb bytes.Buffer
	a := c.app(&out, &errb)
	a.httpClient = &http.Client{Transport: swapTransport{next: http.DefaultTransport, before: func(r *http.Request) {
		if r.Method != http.MethodPatch {
			return
		}
		patches++
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/v1/tasks/"+id, strings.NewReader(`{"title":"Edited by Bob","version":1}`))
		req.Header.Set("Authorization", "Bearer "+other)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("the other edit: HTTP %d", resp.StatusCode)
		}
	}}}
	code := a.run([]string{"task", "update", id, "--priority", "p1_critical"})
	if code != exitConflict {
		t.Fatalf("exit %d, want %d\n%s", code, exitConflict, errb.String())
	}
	if !strings.Contains(errb.String(), "version 2") || !strings.Contains(errb.String(), "Nothing was changed") {
		t.Errorf("stderr should give the current version: %q", errb.String())
	}
	if patches != 1 {
		t.Errorf("the CLI sent %d PATCH requests; it must send one and not retry", patches)
	}
	show := c.ok("task", "show", id)
	if !strings.Contains(show, "Edited by Bob") || strings.Contains(show, "p1_critical") {
		t.Errorf("the task after the conflict:\n%s", show)
	}
}

func TestTaskDecompose(t *testing.T) {
	c, srv := local(t)
	id := c.add("Big job")
	code, _, errb := c.run("task", "decompose", id)
	if code != exitUnexpected || !strings.Contains(errb, "agent") {
		t.Errorf("agents off: exit %d, %q", code, errb)
	}
	srv.EnableAgents(t)
	out := c.ok("task", "decompose", id)
	if !strings.Contains(out, "decomposition queued") || !strings.Contains(out, "pending") {
		t.Errorf("decompose: %s", out)
	}
	if show := c.ok("task", "show", id, "--subtasks", "--agent-runs"); !strings.Contains(show, "subtasks:") || !strings.Contains(show, "decomposer") {
		t.Errorf("show: %s", show)
	}
}

func TestTaskCommandsNeedCredentials(t *testing.T) {
	c, _ := local(t)
	delete(c.env, "TASKIFY_TOKEN")
	if code, _, errb := c.run("task", "list"); code != exitAuth || !strings.Contains(errb, "taskctl login") {
		t.Errorf("no credentials: exit %d, %q", code, errb)
	}
	c.env["TASKIFY_TOKEN"] = "not-a-token"
	if code, _, _ := c.run("task", "list"); code != exitAuth {
		t.Errorf("a bad token: exit %d", code)
	}
	delete(c.env, "TASKIFY_SERVER")
	if code, _, _ := c.run("task", "list"); code != exitUsage {
		t.Errorf("no server: exit %d", code)
	}
}
