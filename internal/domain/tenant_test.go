package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateTenantID(t *testing.T) {
	for _, id := range []string{"tenant_dev", "tenant_acme", "a", "0", "Acme", "Default", "DEFAULT", "acme.corp", "acme-1_b.c",
		strings.Repeat("t", 64)} {
		if err := ValidateTenantID(id); err != nil {
			t.Errorf("%q refused: %v", id, err)
		}
	}
	for _, id := range []string{"", "default", " tenant_acme", "tenant_acme ", " ", "_acme", "-acme", ".acme", "acme corp",
		"acme/1", "acme:1", "ténant", "tenant\n", strings.Repeat("t", 65)} {
		if err := ValidateTenantID(id); !errors.Is(err, ErrValidation) {
			t.Errorf("%q accepted: %v", id, err)
		}
	}
	if err := ValidateTenantID("default"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("default: %v", err)
	}
}

// TestTenantIDsAreNeverTrimmed: every constructor judges the tenant ID as
// given.
func TestTenantIDsAreNeverTrimmed(t *testing.T) {
	if _, _, err := NewTask(NewTaskInput{TenantID: "tenant_acme ", WorkspaceID: "w", Title: "x"}, alice, t0); !errors.Is(err, ErrValidation) {
		t.Errorf("NewTask: %v", err)
	}
	if _, err := NewWorkspace(" tenant_acme", "w", "n", t0); !errors.Is(err, ErrValidation) {
		t.Errorf("NewWorkspace: %v", err)
	}
	if _, err := NewAgentJob(NewAgentJobInput{TenantID: "tenant acme", AgentType: AgentTriage, DedupKey: "k"}, t0); !errors.Is(err, ErrValidation) {
		t.Errorf("NewAgentJob: %v", err)
	}
	if _, err := NewAgentJob(NewAgentJobInput{TenantID: "default", AgentType: AgentTriage, DedupKey: "k"}, t0); !errors.Is(err, ErrValidation) {
		t.Errorf("NewAgentJob with default: %v", err)
	}
	task, _, err := NewTask(NewTaskInput{TenantID: "Tenant.A", WorkspaceID: "w", Title: "x"}, alice, t0)
	if err != nil || task.TenantID != "Tenant.A" {
		t.Errorf("case kept: %v, %v", task, err)
	}
}
