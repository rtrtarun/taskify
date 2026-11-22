package domain

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestApprovalRequestedEvent(t *testing.T) {
	task := newTaskT(t, "Fix checkout", "usr_bob", PriorityHigh)
	job := &AgentJob{ID: uuid.New(), AgentType: AgentTriage}
	e := NewApprovalRequestedEvent(task, job, []string{"UpdateTaskAttributes", "ShiftDueDate"}, t0)
	if e.Actor.Kind != ActorAgent || e.Actor.ID != "agent:triage" {
		t.Errorf("actor = %+v", e.Actor)
	}
	ns := e.Notifications(Links{API: "https://taskify.test"})
	if len(ns) != 1 || ns[0].UserID != "usr_bob" {
		t.Fatalf("notifications = %+v", ns)
	}
	n := ns[0]
	if want := "taskify:tenant_acme:" + task.ID.String() + ":agent.approval_requested:" + job.ID.String(); n.IdempotencyKey != want {
		t.Errorf("key = %q", n.IdempotencyKey)
	}
	if n.Title != "Approval needed: Fix checkout" || !strings.Contains(n.Body, "2 changes") ||
		!strings.Contains(n.Body, "UpdateTaskAttributes, ShiftDueDate") || n.PriorityHint != "high" {
		t.Errorf("notification = %+v", n)
	}
	urls := n.Payload["actionUrls"].(map[string]string)
	base := "https://taskify.test/api/v1/agent-jobs/" + job.ID.String()
	if urls["approve"] != base+"/apply" || urls["reject"] != base+"/dismiss" || urls["view"] != base || len(urls) != 3 {
		t.Errorf("actionUrls = %v", urls)
	}
	if n.Payload["jobId"] != job.ID.String() || n.Payload["agentType"] != "triage" {
		t.Errorf("payload = %v", n.Payload)
	}

	// Unassigned: the creator decides.
	unassigned := newTaskT(t, "Loose end", "", "")
	if ns := NewApprovalRequestedEvent(unassigned, job, []string{"X"}, t0).Notifications(Links{API: "https://t"}); len(ns) != 1 ||
		ns[0].UserID != unassigned.CreatorID || !strings.Contains(ns[0].Body, "1 change ") {
		t.Errorf("unassigned = %+v", ns)
	}
}

func TestCanDecide(t *testing.T) {
	task := newTaskT(t, "T", "usr_bob", "")
	for user, want := range map[string]bool{"usr_bob": true, task.CreatorID: true, "usr_eve": false, "": false} {
		if got := task.CanDecide(user, false); got != want {
			t.Errorf("CanDecide(%q) = %v", user, got)
		}
	}
	if !task.CanDecide("usr_eve", true) {
		t.Error("an admin cannot decide")
	}
}
