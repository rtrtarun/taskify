package repotest

import (
	"testing"

	"github.com/rtrtarun/taskify/internal/domain"
)

// stored creates a task in the backend and moves it to status through
// legal transitions.
func stored(t *testing.T, b Backend, tenantID string, status domain.Status, mutate func(*domain.NewTaskInput)) *domain.Task {
	t.Helper()
	task := newTask(t, tenantID, mutate)
	create(t, b, task)
	path := map[domain.Status][]domain.Status{
		domain.StatusInProgress: {domain.StatusInProgress},
		domain.StatusBlocked:    {domain.StatusInProgress, domain.StatusBlocked},
		domain.StatusCompleted:  {domain.StatusCompleted},
		domain.StatusArchived:   {domain.StatusArchived},
	}[status]
	for _, s := range path {
		before := task.Version
		if _, err := task.TransitionTo(s, alice, t0); err != nil {
			t.Fatal(err)
		}
		if err := b.Tasks.Update(ctx, task, before); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

// TestCountAssigned covers the load query of the digest gatekeeper (M10-01).
func TestCountAssigned(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		seedWorkspace(t, b, "tenant_other")
		other, _ := domain.NewWorkspace(tenant, "ws_other", "Other", t0)
		if err := b.Workspaces.Create(ctx, other); err != nil {
			t.Fatal(err)
		}
		critical := func(in *domain.NewTaskInput) { in.Priority = domain.PriorityCritical }
		stored(t, b, tenant, domain.StatusInProgress, critical)
		stored(t, b, tenant, domain.StatusInProgress, func(in *domain.NewTaskInput) { critical(in); in.WorkspaceID = "ws_other" })
		// None of these count for bob.
		stored(t, b, tenant, domain.StatusTodo, critical)
		stored(t, b, tenant, domain.StatusBlocked, critical)
		stored(t, b, tenant, domain.StatusCompleted, critical)
		stored(t, b, tenant, domain.StatusInProgress, func(in *domain.NewTaskInput) { in.Priority = domain.PriorityHigh })
		stored(t, b, tenant, domain.StatusInProgress, func(in *domain.NewTaskInput) { critical(in); in.AssigneeID = "usr_carol" })
		stored(t, b, "tenant_other", domain.StatusInProgress, critical)

		n, err := b.Tasks.CountAssigned(ctx, tenant, "usr_bob", domain.StatusInProgress, domain.PriorityCritical)
		if err != nil || n != 2 {
			t.Errorf("bob's in-progress critical tasks = %d, %v; want 2 across workspaces", n, err)
		}
		if n, _ := b.Tasks.CountAssigned(ctx, tenant, "usr_carol", domain.StatusInProgress, domain.PriorityCritical); n != 1 {
			t.Errorf("carol = %d, want 1", n)
		}
		if n, _ := b.Tasks.CountAssigned(ctx, tenant, "usr_nobody", domain.StatusInProgress, domain.PriorityCritical); n != 0 {
			t.Errorf("nobody = %d, want 0", n)
		}
	})
}
