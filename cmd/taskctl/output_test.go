package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/tests/localserver"
)

// get fetches a path as the CLI's user would, for comparing bodies.
func get(t *testing.T, c *cli, path string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, c.env["TASKIFY_SERVER"]+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.env["TASKIFY_TOKEN"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return b
}

func TestJSONIsTheAPIBodyUnchanged(t *testing.T) {
	c, srv := local(t)
	id := c.add("Machine readable", "--desc", "line one\nline two", "--label", "a")
	c.ok("task", "comment", id, "A comment")
	job := srv.Proposal(t, uuid.MustParse(id), "Proposed text")
	c.env["TASKIFY_TOKEN"] = srv.Token(t, "usr_admin", "admin")
	c.add("Second", "--assignee", "usr_bob")
	srv.DeadLetter(t)

	cases := []struct {
		name string
		args []string
		path string
	}{
		{"show", []string{"task", "show", id}, "/api/v1/tasks/" + id},
		{"list", []string{"task", "list", "--limit", "10"}, "/api/v1/tasks?limit=10"},
		{"job show", []string{"job", "show", job.String()}, "/api/v1/agent-jobs/" + job.String()},
		{"job list", []string{"job", "list", "--status", "awaiting_approval"}, "/api/v1/agent-jobs?status=awaiting_approval"},
		{"outbox list", []string{"admin", "outbox", "list", "--status", "dead_letter"}, "/api/v1/admin/outbox?status=dead_letter"},
	}
	for _, tc := range cases {
		out := c.ok(append([]string{"--json"}, tc.args...)...)
		want := get(t, c, tc.path)
		if !json.Valid([]byte(out)) {
			t.Errorf("%s: output is not JSON: %s", tc.name, out)
		}
		if strings.TrimSpace(out) != strings.TrimSpace(string(want)) {
			t.Errorf("%s: --json differs from the API body\n got: %s\nwant: %s", tc.name, out, want)
		}
	}

	// Writes print the body the server returned for them.
	out := c.ok("--json", "task", "update", id, "--title", "Renamed")
	var task map[string]any
	if err := json.Unmarshal([]byte(out), &task); err != nil || task["title"] != "Renamed" || task["version"] != 2.0 {
		t.Errorf("--json update: %s (%v)", out, err)
	}
	// The flag may come after the subcommand.
	if out := c.ok("task", "show", id, "--json"); !json.Valid([]byte(out)) {
		t.Errorf("trailing --json: %s", out)
	}
	var v map[string]string
	if err := json.Unmarshal([]byte(c.ok("--json", "version")), &v); err != nil || v["version"] != version {
		t.Errorf("version --json: %v %v", v, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(c.ok("--json", "config", "view")), &cfg); err != nil || cfg["credentials"] != "set (environment)" {
		t.Errorf("config view --json: %v %v", cfg, err)
	}
}

func TestQuietPrintsIDsOnly(t *testing.T) {
	c, srv := local(t)
	a := c.add("One")
	b := c.add("Two")
	if out := c.ok("--quiet", "task", "add", "Three", "--workspace", localserver.Workspace); !fullUUID.MatchString(strings.TrimSpace(out)) {
		t.Errorf("task add --quiet: %q", out)
	}
	ids := strings.Fields(c.ok("-q", "task", "list"))
	if len(ids) != 3 || !slices.Contains(ids, a) || !slices.Contains(ids, b) {
		t.Errorf("task list --quiet: %v", ids)
	}
	job := srv.Proposal(t, uuid.MustParse(a), "x")
	if out := strings.TrimSpace(c.ok("-q", "job", "list")); out != job.String() {
		t.Errorf("job list --quiet: %q", out)
	}
	if out := c.ok("-q", "task", "update", a, "--title", "Renamed"); strings.TrimSpace(out) != a {
		t.Errorf("update --quiet: %q", out)
	}
	if code, _, _ := c.run("--json", "--quiet", "task", "list"); code != exitUsage {
		t.Errorf("--json with --quiet: exit %d", code)
	}
}

func TestJobWatchAndDecomposeWait(t *testing.T) {
	c, srv := local(t)
	srv.EnableAgents(t)

	runWith := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		a := c.app(&out, &errb)
		a.pollEvery = 5 * time.Millisecond
		code := a.run(args)
		return code, out.String(), errb.String()
	}

	// show --watch follows a queued job to the point it waits for approval.
	id := c.add("Watched")
	job := strings.TrimSpace(c.ok("-q", "task", "decompose", id))
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = srv.Advance(uuid.MustParse(job), domain.JobRunning, "")
		time.Sleep(60 * time.Millisecond)
		_ = srv.Advance(uuid.MustParse(job), domain.JobAwaitingApproval, "")
	}()
	code, out, errb := runWith("job", "show", "--watch", job)
	if code != 0 {
		t.Fatalf("watch: exit %d: %s", code, errb)
	}
	i1, i2, i3 := strings.Index(out, "-- pending --"), strings.Index(out, "-- running --"), strings.Index(out, "-- awaiting_approval --")
	if i1 < 0 || i2 <= i1 || i3 <= i2 {
		t.Errorf("watch output does not show each change in order:\n%s", out)
	}

	// A job that never finishes: --timeout ends the watch.
	stuck := strings.TrimSpace(c.ok("-q", "task", "decompose", c.add("Stuck")))
	if code, _, errb := runWith("job", "show", "--watch", "--timeout", "50ms", stuck); code != exitUnexpected || !strings.Contains(errb, "still running") {
		t.Errorf("watch timeout: exit %d, %q", code, errb)
	}

	// decompose --wait returns when the job stops; a failed job is exit 1.
	for name, tc := range map[string]struct {
		to   domain.JobStatus
		msg  string
		want int
	}{"completed": {domain.JobCompleted, "", 0}, "failed": {domain.JobFailed, "the model was unreachable", exitUnexpected}} {
		tid := c.add("Wait " + name)
		go func() {
			// Creating the task also queued a triage job; wait for the
			// decomposer's.
			for range 300 {
				time.Sleep(10 * time.Millisecond)
				jobs, _ := srv.JobsOf(uuid.MustParse(tid))
				for _, j := range jobs {
					if j.AgentType == domain.AgentDecomposer {
						time.Sleep(30 * time.Millisecond)
						if err := srv.Advance(j.ID, tc.to, tc.msg); err != nil {
							t.Errorf("advance: %v", err)
						}
						return
					}
				}
			}
		}()
		code, out, errb := runWith("task", "decompose", tid, "--wait")
		if code != tc.want || field(out, "status") != string(tc.to) {
			t.Errorf("--wait %s: exit %d\nstdout: %s\nstderr: %s", name, code, out, errb)
		}
		if tc.msg != "" && !strings.Contains(errb, tc.msg) {
			t.Errorf("--wait %s: the failure reason is not reported: %q", name, errb)
		}
	}
}

func TestCompletionVersionAndUserAgent(t *testing.T) {
	c := newCLI(t)
	for _, shell := range []string{"bash", "zsh", "powershell"} {
		code, out, errb := c.run("completion", shell)
		if code != 0 || len(out) < 200 || !strings.Contains(out, "taskctl") {
			t.Errorf("completion %s: exit %d, %d bytes, %q", shell, code, len(out), errb)
		}
	}
	if code, out, _ := c.run("version"); code != 0 || strings.TrimSpace(out) != "taskctl "+version {
		t.Errorf("version: %d %q", code, out)
	}
	var ua string
	url := serve(t, 200, meBody, func(r *http.Request) { ua = r.Header.Get("User-Agent") })
	c.stdin = "tok-abcdefghij\n"
	c.run("login", "--server", url)
	if ua != "taskctl/"+version {
		t.Errorf("User-Agent = %q", ua)
	}
}
