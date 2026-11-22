package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

type faultWorkspaces struct {
	repository.WorkspaceRepository
	f *faults
}

func (r faultWorkspaces) ListLearningTenants(ctx context.Context, week string, limit int) ([]domain.LearningTenant, error) {
	if err := r.f.hit("Workspaces.ListLearningTenants"); err != nil {
		return nil, err
	}
	return r.WorkspaceRepository.ListLearningTenants(ctx, week, limit)
}

// learningWorkspace creates workspace ws in tenantID with agents on, so the
// tenant is due a learning job.
func learningWorkspace(t *testing.T, b repotest.Backend, tenantID string) {
	t.Helper()
	ws, err := domain.NewWorkspace(tenantID, "ws", "W", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.UpdateAgentSettings(domain.AgentSettingsPatch{AgentsEnabled: ptr(true)}, t0); err != nil {
		t.Fatal(err)
	}
	if err := b.Workspaces.Create(ctx, ws); err != nil {
		t.Fatal(err)
	}
}

// TestSchedulerFailedLearningScanIsRetried: the week is remembered only
// once a learning scan runs to the end, so a failed scan queues nothing
// and the next tick queues the tenant's job (G16).
func TestSchedulerFailedLearningScanIsRetried(t *testing.T) {
	for _, step := range []string{"Workspaces.ListLearningTenants", "Jobs.Insert"} {
		t.Run(step, func(t *testing.T) {
			each(t, func(t *testing.T, b repotest.Backend) {
				learningWorkspace(t, b, tenant)
				now := time.Date(2026, 11, 23, 0, 30, 0, 0, time.UTC)
				fs := newFaults()
				fs.fail(step, 0, 1)
				s := NewScheduler(b.Tx, b.Tasks, b.Outbox, SchedulerConfig{Logger: quiet, Now: func() time.Time { return now },
					Jobs: faultJobs{b.AgentJobs, fs}, Workspaces: faultWorkspaces{b.Workspaces, fs}, Learning: true})

				if _, err := s.Tick(ctx); !errors.Is(err, errInjected) {
					t.Fatalf("tick = %v, want the injected failure", err)
				}
				if got := queuedLearning(t, b, now); len(got) != 0 {
					t.Fatalf("the failed scan queued %v", got)
				}
				if _, err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				if got := queuedLearning(t, b, now); len(got) != 1 || got[0] != "learning:tenant_acme:week:2026-W48@ws" {
					t.Fatalf("after the retry: %v", got)
				}
				scans := fs.count("Workspaces.ListLearningTenants")
				if _, err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				if got := fs.count("Workspaces.ListLearningTenants"); got != scans {
					t.Errorf("a later tick that week scanned again: %d scans, want %d", got, scans)
				}
			})
		})
	}
}

// heldTenants answers ListLearningTenants from a list read earlier, as a
// replica's query does while another replica's scan has queued those jobs
// but not yet committed.
type heldTenants struct {
	repository.WorkspaceRepository
	held  []domain.LearningTenant
	calls int
}

func (h *heldTenants) ListLearningTenants(ctx context.Context, week string, limit int) ([]domain.LearningTenant, error) {
	h.calls++
	if h.held != nil {
		return h.held, nil
	}
	return h.WorkspaceRepository.ListLearningTenants(ctx, week, limit)
}

// TestLearningScanYieldsToAnotherReplica: a full batch of tenants whose
// jobs another replica queued first adds nothing; the scan stops, does not
// remember the week, and looks again on the next tick.
func TestLearningScanYieldsToAnotherReplica(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		learningWorkspace(t, b, tenant)
		now := time.Date(2026, 11, 23, 0, 30, 0, 0, time.UTC)
		cfg := SchedulerConfig{BatchSize: 1, Logger: quiet, Now: func() time.Time { return now }, Jobs: b.AgentJobs, Learning: true}

		held, err := b.Workspaces.ListLearningTenants(ctx, domain.LearningWeek(now), 1)
		if err != nil || len(held) != 1 {
			t.Fatalf("tenants = %v, %v", held, err)
		}
		first := cfg
		first.Workspaces = b.Workspaces
		if _, err := NewScheduler(b.Tx, b.Tasks, b.Outbox, first).Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if got := queuedLearning(t, b, now); len(got) != 1 {
			t.Fatalf("first replica queued %v", got)
		}

		ws := &heldTenants{WorkspaceRepository: b.Workspaces, held: held}
		late := cfg
		late.Workspaces = ws
		s := NewScheduler(b.Tx, b.Tasks, b.Outbox, late)
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if ws.calls != 1 {
			t.Errorf("the scan read %d batches, want it to stop after the first", ws.calls)
		}
		ws.held = nil
		for i := 0; i < 2; i++ {
			if _, err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if ws.calls != 2 {
			t.Errorf("scans = %d, want one more, then none that week", ws.calls)
		}
		if got := queuedLearning(t, b, now); len(got) != 0 {
			t.Errorf("second replica queued %v", got)
		}
	})
}
