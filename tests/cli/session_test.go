// Package cli runs the real taskctl binary against Taskify started in-process
// (tests/localserver), the way a person or a script would use it. No running
// process and no database are needed, so it runs in CI.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rtrtarun/taskify/tests/localserver"
)

// build compiles taskctl into a temporary directory.
func build(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..")
	bin := filepath.Join(t.TempDir(), "taskctl")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/taskctl")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

type session struct {
	t   *testing.T
	bin string
	env []string
}

func (s session) run(args ...string) (stdout string, exitCode int) {
	s.t.Helper()
	cmd := exec.Command(s.bin, args...)
	cmd.Env = s.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		s.t.Fatalf("run %v: %v", args, err)
	}
	if code != 0 {
		s.t.Logf("taskctl %v: exit %d, stderr: %s", args, code, errb.String())
	}
	return out.String(), code
}

func (s session) json(args ...string) map[string]any {
	s.t.Helper()
	out, code := s.run(append([]string{"--json"}, args...)...)
	if code != 0 {
		s.t.Fatalf("taskctl %v: exit %d", args, code)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		s.t.Fatalf("taskctl %v: not JSON: %v\n%s", args, err, out)
	}
	return v
}

// TestScriptedSession is the M13 exit criterion: create a task, comment on
// it, complete it, and read it back with --json.
func TestScriptedSession(t *testing.T) {
	srv := localserver.Start(t)
	dir := t.TempDir()
	s := session{t: t, bin: build(t), env: append(os.Environ(),
		"TASKIFY_SERVER="+srv.URL,
		"TASKIFY_TOKEN="+srv.Token(t, "usr_ann", "member"),
		"TASKIFY_CONFIG="+filepath.Join(dir, "config.yaml"),
		"TASKIFY_PROFILE=session",
	)}

	created := s.json("task", "add", "Ship the CLI", "--workspace", localserver.Workspace, "--priority", "p2_high", "--label", "m13")
	id, _ := created["id"].(string)
	if id == "" || created["status"] != "todo" || created["version"] != 1.0 {
		t.Fatalf("created: %v", created)
	}

	comment := s.json("task", "comment", id[:8], "First draft is in review")
	if comment["task_id"] != id || comment["author_id"] != "usr_ann" {
		t.Errorf("comment: %v", comment)
	}

	if done := s.json("task", "done", id[:8]); done["status"] != "completed" || done["completed_at"] == nil {
		t.Errorf("done: %v", done)
	}

	got := s.json("task", "show", id)
	if got["id"] != id || got["status"] != "completed" || got["title"] != "Ship the CLI" || got["priority"] != "p2_high" {
		t.Errorf("read back: %v", got)
	}
	comments, _ := got["comments"].([]any)
	if len(comments) != 1 || comments[0].(map[string]any)["body"] != "First draft is in review" {
		t.Errorf("comments: %v", got["comments"])
	}

	// The binary's exit codes reach the shell.
	if _, code := s.run("task", "show", "0193a1b2-c3d4-7e5f-8a9b-0123456789ab"); code != 4 {
		t.Errorf("unknown task: exit %d, want 4", code)
	}
	if _, code := s.run("task", "frobnicate"); code != 2 {
		t.Errorf("usage error: exit %d, want 2", code)
	}
	if out, code := s.run("version"); code != 0 || !strings.HasPrefix(out, "taskctl ") {
		t.Errorf("version: %d %q", code, out)
	}
}
