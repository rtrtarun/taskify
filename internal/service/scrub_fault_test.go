package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

// faultBriefs also fails the brief calls of a permanent delete.
type faultBriefs struct{ faultJobs }

func (f faultBriefs) LockBriefs(ctx context.Context, tenantID string, exclusive bool) error {
	if err := f.in.hit(); err != nil {
		return err
	}
	return f.AgentJobRepository.LockBriefs(ctx, tenantID, exclusive)
}

func (f faultBriefs) DeleteBriefsMentioning(ctx context.Context, tenantID string, taskID uuid.UUID) ([]uuid.UUID, error) {
	if err := f.in.hit(); err != nil {
		return nil, err
	}
	return f.AgentJobRepository.DeleteBriefsMentioning(ctx, tenantID, taskID)
}

// TestDeleteTaskScrubFailuresLeaveNoPartialChange (AP-08): the task, the
// briefs that name it, and their outbox rows go in one transaction, so a
// failure at any step keeps them all, and its dependent stays blocked.
func TestDeleteTaskScrubFailuresLeaveNoPartialChange(t *testing.T) {
	day := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	for failAt := 1; ; failAt++ {
		b := repotest.Memory()
		f := newFixture(t, b, nil)
		task := f.create(t, func(in *domain.NewTaskInput) { in.Title = "Acquire Initech quietly" })
		dep := f.create(t, nil)
		f.link(t, dep, task)
		brief := f.storeBrief(t, bob.ID, day, task)
		state := func() string {
			s := snapshot(t, b, []uuid.UUID{task.ID, dep.ID})
			_, err := b.AgentJobs.Get(ctx, tenant, brief.ID)
			rows, _ := b.Outbox.ListForAggregate(ctx, brief.ID)
			return s + fmt.Sprintf("brief: %v rows=%d\n", err, len(rows))
		}
		before := state()

		in := &injector{failAt: failAt}
		s := NewTaskService(Deps{
			Tx: b.Tx, Tasks: faultTasks{b.Tasks, in}, Workspaces: faultWorkspaces{b.Workspaces, in},
			Comments: faultComments{b.Comments, in}, Outbox: faultOutbox{b.Outbox, in},
			Dependencies: faultDeps{b.Deps, in}, AgentJobs: faultBriefs{faultJobs{b.AgentJobs, in}},
		}, Config{PublicBaseURL: "https://taskify.test"}, WithClock(func() time.Time { return f.now }))
		err := s.DeleteTask(ctx, tenant, task.ID)
		if in.calls < failAt {
			if err != nil || failAt == 1 {
				t.Fatalf("clean run: %v after %d calls", err, in.calls)
			}
			if _, err := b.AgentJobs.Get(ctx, tenant, brief.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("clean run kept the brief: %v", err)
			}
			if got := f.get(t, dep.ID); got.Status != domain.StatusTodo {
				t.Errorf("clean run left the dependent %s", got.Status)
			}
			return
		}
		if !errors.Is(err, errInjected) {
			t.Errorf("failing call %d: error = %v, want the injected failure", failAt, err)
		}
		if after := state(); after != before {
			t.Errorf("failing call %d left a partial change:\nbefore:\n%safter:\n%s", failAt, before, after)
		}
	}
}
