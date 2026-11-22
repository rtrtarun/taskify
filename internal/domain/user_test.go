package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// User IDs that every entry point accepts and refuses (AP-11): 1 to 128
// printable ASCII characters, space to tilde.
var (
	validUserIDs = []string{"usr_bob", "a", "0", "alice@example.com", "Bob Smith", "user:42", "~!#$%^&*()[]{}|",
		strings.Repeat("u", MaxUserIDLen)}
	invalidUserIDs = []string{"josé", "用户", "usr\n", "usr\tbob", "usr\x7f", "\x00usr", "usr\r", strings.Repeat("u", MaxUserIDLen+1)}
)

func TestValidateUserID(t *testing.T) {
	for _, id := range validUserIDs {
		if err := ValidateUserID("user_id", id); err != nil {
			t.Errorf("%q refused: %v", id, err)
		}
	}
	for _, id := range append([]string{""}, invalidUserIDs...) {
		if err := ValidateUserID("user_id", id); !errors.Is(err, ErrValidation) {
			t.Errorf("%q accepted: %v", id, err)
		}
	}
	if err := ValidateUserID("assignee_id", "josé"); err == nil || !strings.Contains(err.Error(), "assignee_id must be 1 to 128 printable ASCII") {
		t.Errorf("message = %v", err)
	}
	if err := ValidateUserID("assignee_id", ""); err == nil || !strings.Contains(err.Error(), "assignee_id is required") {
		t.Errorf("message = %v", err)
	}
}

// TestUserIDsInDomainValidation: the assignee, watchers, creator, comment
// author, contact owner, and brief recipient all follow the rule.
func TestUserIDsInDomainValidation(t *testing.T) {
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	newTask := func(creator, assignee string, watchers ...string) error {
		_, _, err := NewTask(NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws", Title: "x", AssigneeID: assignee,
			Watchers: watchers}, User(creator), now)
		return err
	}
	for _, id := range validUserIDs {
		if err := newTask(id, id, id); err != nil {
			t.Errorf("task with user %q: %v", id, err)
		}
	}
	for _, id := range invalidUserIDs {
		if strings.TrimSpace(id) != id {
			continue // task fields are trimmed first, as they always were
		}
		for field, err := range map[string]error{
			"creator_id":   newTask(id, "usr_bob"),
			"assignee_id":  newTask("usr_alice", id),
			"a watcher ID": newTask("usr_alice", "usr_bob", "usr_carol", id),
		} {
			if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), field) {
				t.Errorf("%s %q: %v", field, id, err)
			}
		}
	}

	// An update is checked the same way.
	task, _, err := NewTask(NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws", Title: "x"}, User("usr_alice"), now)
	if err != nil {
		t.Fatal(err)
	}
	bad := "usr\x1b[31m"
	if _, err := task.Apply(Patch{AssigneeID: &bad}, User("usr_alice"), now); !errors.Is(err, ErrValidation) {
		t.Errorf("update to assignee %q: %v", bad, err)
	}
	if _, err := task.Apply(Patch{Watchers: &[]string{"zoë"}}, User("usr_alice"), now); !errors.Is(err, ErrValidation) {
		t.Errorf("update to watcher zoë: %v", err)
	}
	if task.AssigneeID != "" || len(task.Watchers) != 0 || task.Version != 1 {
		t.Errorf("a refused update changed the task: %+v", task)
	}

	// The comment author, the contact owner, and the brief recipient.
	if _, _, err := NewComment(task, "hi", User("josé"), now); !errors.Is(err, ErrValidation) {
		t.Errorf("comment by josé: %v", err)
	}
	if _, _, err := NewComment(task, "hi", Agent(AgentBlocker), now); err != nil {
		t.Errorf("comment by the agent: %v", err)
	}
	if _, err := NewContactEntry(Contact{TenantID: "tenant_acme", UserID: "usr\n"}, 5, now); !errors.Is(err, ErrValidation) {
		t.Errorf("contact of usr\n: %v", err)
	}
	if _, err := NewContactEntry(Contact{TenantID: "tenant_acme", UserID: "Bob Smith"}, 5, now); err != nil {
		t.Errorf("contact of Bob Smith: %v", err)
	}
	if _, err := NewBriefJob(BriefRecipient{TenantID: "tenant_acme", UserID: "josé", WorkspaceID: "ws"}, now, now); !errors.Is(err, ErrValidation) {
		t.Errorf("brief for josé: %v", err)
	}
	if _, err := ParseBriefInput([]byte(`{"trigger":"morning_brief","user_id":"josé","workspace_id":"ws","brief_date":"2026-11-22","brief_at":"2026-11-22T08:00:00Z"}`)); !errors.Is(err, ErrValidation) {
		t.Errorf("brief input for josé: %v", err)
	}
}
