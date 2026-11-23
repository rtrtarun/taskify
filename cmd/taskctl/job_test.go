package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/tests/localserver"
)

func TestJobApproveAppliesATier2Proposal(t *testing.T) {
	c, srv := local(t)
	id := c.add("Needs a description")
	job := srv.Proposal(t, uuid.MustParse(id), "Written by the agent")

	if l := c.ok("job", "list", "--status", "awaiting_approval"); !strings.Contains(l, job.String()) || !strings.Contains(l, "triage") {
		t.Errorf("job list: %s", l)
	}
	if l := c.ok("job", "list", "--task", id[:8]); !strings.Contains(l, job.String()) {
		t.Errorf("job list --task: %s", l)
	}
	if l := c.ok("job", "list", "--status", "completed"); strings.Contains(l, job.String()) {
		t.Errorf("job list --status completed: %s", l)
	}
	show := c.ok("job", "show", job.String())
	if field(show, "status") != "awaiting_approval" || !strings.Contains(show, "Written by the agent") {
		t.Errorf("job show: %s", show)
	}

	out := c.ok("job", "approve", job.String())
	if !strings.Contains(out, "is now completed") {
		t.Errorf("approve: %s", out)
	}
	if task := c.ok("task", "show", id); !strings.Contains(task, "Written by the agent") {
		t.Errorf("the proposal was not applied:\n%s", task)
	}
	if again := c.ok("job", "approve", job.String()); !strings.Contains(again, "nothing changed") {
		t.Errorf("a repeated approve: %s", again)
	}
	if code, _, _ := c.run("job", "reject", job.String()); code != exitUnexpected {
		t.Errorf("reject after approve: exit %d", code)
	}
}

func TestJobRejectAndStale(t *testing.T) {
	c, srv := local(t)
	id := c.add("Task")
	rejected := srv.Proposal(t, uuid.MustParse(id), "Never applied")
	if out := c.ok("job", "reject", rejected.String()); !strings.Contains(out, "is now dismissed") {
		t.Errorf("reject: %s", out)
	}
	if task := c.ok("task", "show", id); strings.Contains(task, "Never applied") {
		t.Errorf("a rejected proposal was applied:\n%s", task)
	}

	// The task moves on after the proposal: approving it is a version conflict.
	stale := srv.Proposal(t, uuid.MustParse(id), "Too late")
	c.ok("task", "update", id, "--title", "Edited meanwhile")
	if code, _, errb := c.run("job", "approve", stale.String()); code != exitConflict {
		t.Errorf("stale approve: exit %d, %q", code, errb)
	}

	for name, args := range map[string][]string{
		"not a job ID": {"job", "show", "abc"},
		"unknown job":  {"job", "show", uuid.NewString()},
	} {
		want := map[string]int{"not a job ID": exitUsage, "unknown job": exitNotFound}[name]
		if code, _, _ := c.run(args...); code != want {
			t.Errorf("%s: exit %d, want %d", name, code, want)
		}
	}
}

func TestJobDecisionNeedsAPersonWhoMay(t *testing.T) {
	c, srv := local(t)
	id := c.add("Mine")
	job := srv.Proposal(t, uuid.MustParse(id), "Proposed")
	// usr_eve is neither the creator, the assignee, nor an admin.
	c.env["TASKIFY_TOKEN"] = srv.Token(t, "usr_eve", "member")
	if code, _, _ := c.run("job", "approve", job.String()); code != exitAuth {
		t.Errorf("a stranger approving: exit %d", code)
	}
}

func TestAdminCommandsRefuseMembers(t *testing.T) {
	c, srv := local(t)
	c.add("A task, so the outbox has a row", "--assignee", "usr_bob")
	row := srv.DeadLetter(t)
	for _, args := range [][]string{
		{"admin", "suggestions", "list"},
		{"admin", "suggestions", "decide", "sg_1", "--approve"},
		{"admin", "rules", "bootstrap", localserver.Workspace},
		{"admin", "outbox", "list"},
		{"admin", "outbox", "replay", row.String()},
	} {
		if code, _, errb := c.run(args...); code != exitAuth {
			t.Errorf("%v as a member: exit %d, want %d (%s)", args, code, exitAuth, errb)
		}
	}
}

func TestAdminCommands(t *testing.T) {
	c, srv := local(t)
	c.add("Assigned to bob", "--assignee", "usr_bob") // raises task.created, so the outbox has a row
	c.env["TASKIFY_TOKEN"] = srv.Token(t, "usr_admin", "admin")

	row := srv.DeadLetter(t)
	if l := c.ok("admin", "outbox", "list", "--status", "dead_letter"); !strings.Contains(l, row.String()) || !strings.Contains(l, "503 from AgentPulse") {
		t.Errorf("outbox list: %s", l)
	}
	if out := c.ok("admin", "outbox", "replay", row.String()); !strings.Contains(out, "is pending again") {
		t.Errorf("replay: %s", out)
	}
	if l := c.ok("admin", "outbox", "list", "--status", "dead_letter"); strings.Contains(l, row.String()) {
		t.Errorf("replayed row still dead: %s", l)
	}
	if code, _, _ := c.run("admin", "outbox", "replay", row.String()); code != exitUnexpected { // not a dead letter now
		t.Errorf("replay of a pending row: exit %d", code)
	}
	if code, _, _ := c.run("admin", "outbox", "replay", "nope"); code != exitUsage {
		t.Errorf("replay of a malformed ID: exit %d", code)
	}

	if out := c.ok("admin", "rules", "bootstrap", localserver.Workspace); !strings.Contains(out, "RULE") {
		t.Errorf("bootstrap: %s", out)
	}
	if code, _, _ := c.run("admin", "rules", "bootstrap", "ws_missing"); code != exitNotFound {
		t.Errorf("bootstrap of an unknown workspace: exit %d", code)
	}

	sg := srv.Mock.SeedSuggestion(map[string]any{"tenantId": localserver.Tenant,
		"eventPattern": map[string]any{"eventType": "task.updated", "label": nil}, "currentAdjustment": 0.0,
		"proposedAdjustment": -5.0, "signalCount": 7.0, "rationale": "Dismissed by most."})
	if l := c.ok("admin", "suggestions", "list"); !strings.Contains(l, sg) || !strings.Contains(l, "task.updated") || !strings.Contains(l, "+0 -> -5") {
		t.Errorf("suggestions list: %s", l)
	}
	if code, _, _ := c.run("admin", "suggestions", "decide", sg); code != exitUsage {
		t.Errorf("decide without a choice: exit %d", code)
	}
	if code, _, _ := c.run("admin", "suggestions", "decide", sg, "--approve", "--reject"); code != exitUsage {
		t.Errorf("decide with both: exit %d", code)
	}
	if out := c.ok("admin", "suggestions", "decide", sg, "--approve"); !strings.Contains(out, "is now APPROVED") {
		t.Errorf("decide: %s", out)
	}
}

func TestWorkspaceList(t *testing.T) {
	c, _ := local(t)
	out := c.ok("workspace", "list")
	if !strings.Contains(out, localserver.Workspace) || !strings.Contains(out, "ID") {
		t.Errorf("workspace list: %s", out)
	}
	if q := strings.TrimSpace(c.ok("--quiet", "workspace", "list")); q != localserver.Workspace {
		t.Errorf("--quiet: %q", q)
	}
	var body struct {
		Workspaces []struct{ ID string } `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(c.ok("--json", "workspace", "list")), &body); err != nil || len(body.Workspaces) != 1 {
		t.Errorf("--json: %v %+v", err, body)
	}
}
