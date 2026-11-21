package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
	"github.com/rtrtarun/taskify/internal/repository"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

var errInjected = errors.New("injected failure")

// injector fails the failAt-th repository call (1-based); 0 fails none.
type injector struct {
	failAt, calls int
}

func (in *injector) hit() error {
	in.calls++
	if in.calls == in.failAt {
		return errInjected
	}
	return nil
}

// Wrappers pass every call through, counting the ones the service makes.
type faultTasks struct {
	repository.TaskRepository
	in *injector
}

func (f faultTasks) Create(ctx context.Context, t *domain.Task) error {
	if err := f.in.hit(); err != nil {
		return err
	}
	return f.TaskRepository.Create(ctx, t)
}
func (f faultTasks) Get(ctx context.Context, tenant string, id uuid.UUID) (*domain.Task, error) {
	if err := f.in.hit(); err != nil {
		return nil, err
	}
	return f.TaskRepository.Get(ctx, tenant, id)
}
func (f faultTasks) GetForUpdate(ctx context.Context, tenant string, id uuid.UUID) (*domain.Task, error) {
	if err := f.in.hit(); err != nil {
		return nil, err
	}
	return f.TaskRepository.GetForUpdate(ctx, tenant, id)
}
func (f faultTasks) Update(ctx context.Context, t *domain.Task, v int) error {
	if err := f.in.hit(); err != nil {
		return err
	}
	return f.TaskRepository.Update(ctx, t, v)
}
func (f faultTasks) Delete(ctx context.Context, tenant string, id uuid.UUID) error {
	if err := f.in.hit(); err != nil {
		return err
	}
	return f.TaskRepository.Delete(ctx, tenant, id)
}

type faultDeps struct {
	repository.DependencyRepository
	in *injector
}

func (f faultDeps) LockTenant(ctx context.Context, tenant string) error {
	if err := f.in.hit(); err != nil {
		return err
	}
	return f.DependencyRepository.LockTenant(ctx, tenant)
}
func (f faultDeps) Create(ctx context.Context, d domain.Dependency) (bool, error) {
	if err := f.in.hit(); err != nil {
		return false, err
	}
	return f.DependencyRepository.Create(ctx, d)
}
func (f faultDeps) Reaches(ctx context.Context, tenant string, from, to uuid.UUID) (bool, error) {
	if err := f.in.hit(); err != nil {
		return false, err
	}
	return f.DependencyRepository.Reaches(ctx, tenant, from, to)
}
func (f faultDeps) ListDependents(ctx context.Context, tenant string, id uuid.UUID) ([]uuid.UUID, error) {
	if err := f.in.hit(); err != nil {
		return nil, err
	}
	return f.DependencyRepository.ListDependents(ctx, tenant, id)
}
func (f faultDeps) HasOpenBlocker(ctx context.Context, tenant string, id uuid.UUID) (bool, error) {
	if err := f.in.hit(); err != nil {
		return false, err
	}
	return f.DependencyRepository.HasOpenBlocker(ctx, tenant, id)
}

type faultComments struct {
	repository.CommentRepository
	in *injector
}

func (f faultComments) Create(ctx context.Context, c domain.Comment) error {
	if err := f.in.hit(); err != nil {
		return err
	}
	return f.CommentRepository.Create(ctx, c)
}

type faultOutbox struct {
	repository.OutboxRepository
	in *injector
}

func (f faultOutbox) Insert(ctx context.Context, e []domain.OutboxEntry) (int, error) {
	if err := f.in.hit(); err != nil {
		return 0, err
	}
	return f.OutboxRepository.Insert(ctx, e)
}
func (f faultOutbox) DeleteForAggregate(ctx context.Context, tenant string, id uuid.UUID) (int, error) {
	if err := f.in.hit(); err != nil {
		return 0, err
	}
	return f.OutboxRepository.DeleteForAggregate(ctx, tenant, id)
}

type faultJobs struct {
	repository.AgentJobRepository
	in *injector
}

func (f faultJobs) Insert(ctx context.Context, j domain.AgentJob) (bool, error) {
	if err := f.in.hit(); err != nil {
		return false, err
	}
	return f.AgentJobRepository.Insert(ctx, j)
}

type faultWorkspaces struct {
	repository.WorkspaceRepository
	in *injector
}

func (f faultWorkspaces) Get(ctx context.Context, tenant, id string) (domain.Workspace, error) {
	if err := f.in.hit(); err != nil {
		return domain.Workspace{}, err
	}
	return f.WorkspaceRepository.Get(ctx, tenant, id)
}

// faultyService is a TaskService whose repositories fail on demand. The
// in-memory store restores its snapshot when a transaction fails, so a
// failed operation must leave no trace.
func faultyService(b repotest.Backend, in *injector, now time.Time) *TaskService {
	return NewTaskService(Deps{
		Tx: b.Tx, Tasks: faultTasks{b.Tasks, in}, Workspaces: faultWorkspaces{b.Workspaces, in},
		Comments: faultComments{b.Comments, in}, Outbox: faultOutbox{b.Outbox, in},
		Dependencies: faultDeps{b.Deps, in}, AgentJobs: faultJobs{b.AgentJobs, in},
	}, Config{PublicBaseURL: "https://taskify.test"}, WithClock(func() time.Time { return now }))
}

// snapshot describes everything a task operation can change.
func snapshot(t *testing.T, b repotest.Backend, ids []uuid.UUID) string {
	t.Helper()
	var sb strings.Builder
	for _, id := range ids {
		task, err := b.Tasks.Get(ctx, tenant, id)
		if err != nil {
			fmt.Fprintf(&sb, "%s: %v\n", id, err)
			continue
		}
		blockers, _ := b.Deps.ListBlockers(ctx, tenant, id)
		comments, _ := b.Comments.ListRecent(ctx, id, 100)
		outbox, _ := b.Outbox.ListForAggregate(ctx, id)
		jobs, _ := b.AgentJobs.ListForTask(ctx, tenant, id)
		fmt.Fprintf(&sb, "%s v%d %s blockers=%v comments=%d outbox=%d jobs=%d\n",
			id, task.Version, task.Status, blockers, len(comments), len(outbox), len(jobs))
	}
	return sb.String()
}

// everyFailure runs op once per repository call it makes, failing that
// call, and checks each failure is reported and leaves the store unchanged.
// It then runs op with no failure and returns the number of calls.
func everyFailure(t *testing.T, name string, setup func(t *testing.T, f *fixture) ([]uuid.UUID, func(s *TaskService) error)) {
	t.Helper()
	for failAt := 1; ; failAt++ {
		b := repotest.Memory()
		f := newFixture(t, b, nil)
		ids, op := setup(t, f)
		before := snapshot(t, b, ids)
		in := &injector{failAt: failAt}
		err := op(faultyService(b, in, f.now))
		if in.calls < failAt {
			if err != nil {
				t.Fatalf("%s: clean run failed: %v", name, err)
			}
			if failAt == 1 {
				t.Fatalf("%s: the operation made no repository call", name)
			}
			return // every call has been failed once
		}
		if !errors.Is(err, errInjected) {
			t.Errorf("%s: failing call %d: error = %v, want the injected failure", name, failAt, err)
		}
		if after := snapshot(t, b, ids); after != before {
			t.Errorf("%s: failing call %d left a partial change:\nbefore:\n%safter:\n%s", name, failAt, before, after)
		}
	}
}

func TestFailuresLeaveNoPartialChange(t *testing.T) {
	ids := func(ts ...*domain.Task) []uuid.UUID {
		var out []uuid.UUID
		for _, x := range ts {
			out = append(out, x.ID)
		}
		return out
	}
	everyFailure(t, "add dependency to a todo task", func(t *testing.T, f *fixture) ([]uuid.UUID, func(*TaskService) error) {
		dep, blocker := f.create(t, nil), f.create(t, nil)
		return ids(dep, blocker), func(s *TaskService) error { _, err := s.AddDependency(ctx, tenant, dep.ID, blocker.ID); return err }
	})
	everyFailure(t, "add dependency to a started task", func(t *testing.T, f *fixture) ([]uuid.UUID, func(*TaskService) error) {
		dep, blocker := f.create(t, nil), f.create(t, nil)
		f.setStatus(t, dep, domain.StatusInProgress)
		return ids(dep, blocker), func(s *TaskService) error { _, err := s.AddDependency(ctx, tenant, dep.ID, blocker.ID); return err }
	})
	everyFailure(t, "complete a blocker", func(t *testing.T, f *fixture) ([]uuid.UUID, func(*TaskService) error) {
		blocker := f.create(t, nil)
		auto, manual, started := f.create(t, nil), f.create(t, nil), f.create(t, nil)
		f.link(t, auto, blocker)
		f.setStatus(t, manual, domain.StatusBlocked)
		f.link(t, manual, blocker)
		f.setStatus(t, started, domain.StatusInProgress)
		f.link(t, started, blocker)
		cur := f.get(t, blocker.ID)
		return ids(blocker, auto, manual, started), func(s *TaskService) error {
			_, err := s.CompleteTask(ctx, alice, tenant, blocker.ID, &cur.Version)
			return err
		}
	})
	everyFailure(t, "restore a blocker", func(t *testing.T, f *fixture) ([]uuid.UUID, func(*TaskService) error) {
		blocker := f.create(t, nil)
		todo, started := f.create(t, nil), f.create(t, nil)
		f.link(t, todo, blocker)
		f.setStatus(t, started, domain.StatusInProgress)
		f.link(t, started, blocker)
		f.setStatus(t, blocker, domain.StatusArchived)
		return ids(blocker, todo, started), func(s *TaskService) error {
			_, err := s.RestoreTask(ctx, alice, tenant, blocker.ID, nil)
			return err
		}
	})
	everyFailure(t, "delete a blocker", func(t *testing.T, f *fixture) ([]uuid.UUID, func(*TaskService) error) {
		blocker, dep := f.create(t, nil), f.create(t, nil)
		f.link(t, dep, blocker)
		return ids(blocker, dep), func(s *TaskService) error { return s.DeleteTask(ctx, tenant, blocker.ID) }
	})
	everyFailure(t, "a person's edit with agents on", func(t *testing.T, f *fixture) ([]uuid.UUID, func(*TaskService) error) {
		f.enableAgents(t)
		task := f.create(t, nil)
		return ids(task), func(s *TaskService) error {
			_, err := s.UpdateTask(ctx, alice, tenant, task.ID, task.Version, domain.Patch{Title: ptr("Renamed")})
			return err
		}
	})
	everyFailure(t, "add a comment", func(t *testing.T, f *fixture) ([]uuid.UUID, func(*TaskService) error) {
		task := f.create(t, nil)
		return ids(task), func(s *TaskService) error { _, err := s.AddComment(ctx, alice, tenant, task.ID, "hello"); return err }
	})
}

func TestCreateFailuresWithAgentsOn(t *testing.T) {
	// A failed create leaves no task; check by listing.
	for failAt := 1; failAt <= 4; failAt++ {
		b := repotest.Memory()
		f := newFixture(t, b, nil)
		f.enableAgents(t)
		in := &injector{failAt: failAt}
		_, err := faultyService(b, in, f.now).CreateTask(ctx, alice, domain.NewTaskInput{TenantID: tenant, WorkspaceID: "ws_core", Title: "T"})
		if in.calls < failAt {
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if !errors.Is(err, errInjected) {
			t.Errorf("failing call %d: %v", failAt, err)
		}
		page, _ := b.Tasks.List(ctx, repository.TaskFilter{TenantID: tenant, Limit: 10})
		if len(page.Tasks) != 0 {
			t.Errorf("failing call %d left a task", failAt)
		}
	}
}

func TestServiceOptionsAndSmallPaths(t *testing.T) {
	b := repotest.Memory()
	m := observability.NewMetrics(nil)
	s := NewTaskService(Deps{Tx: b.Tx, Tasks: b.Tasks, Workspaces: b.Workspaces, Comments: b.Comments, Outbox: b.Outbox,
		Dependencies: b.Deps, AgentJobs: b.AgentJobs}, Config{}, WithMetrics(m))
	if s.cfg.OutboxMaxAttempts != 10 || s.metrics != m || s.now == nil {
		t.Errorf("defaults = %+v", s.cfg)
	}
	if got := (&VersionConflictError{Current: 4}).Error(); !strings.Contains(got, "4") {
		t.Errorf("Error() = %q", got)
	}
	if a := actorOf([]domain.Event{{Actor: domain.System}}); a != (domain.Actor{}) {
		t.Errorf("actorOf without a person = %+v", a)
	}
	w := NewWebhookService(b.Tx, b.Deliveries, s, nil, nil, nil)
	if w.logger == nil || w.now == nil {
		t.Error("webhook service defaults missing")
	}
	fb := NewFeedbackService(b.Tasks, nil, nil)
	if err := fb.Record(ctx, FeedbackInput{TenantID: tenant, UserID: "u", TaskID: uuid.New(), Signal: SignalActed,
		NotificationID: strings.Repeat("n", 129)}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("long notification ID: %v", err)
	}
}

func TestWorkspaceSettingsEdges(t *testing.T) {
	b := repotest.Memory()
	ws := NewWorkspaceService(b.Tx, b.Workspaces, b.Rules, nil, func() time.Time { return t0 })
	if _, _, _, err := ws.Create(ctx, tenant, "", "No id"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("invalid workspace: %v", err)
	}
	newFixture(t, b, nil)
	// An empty patch changes nothing; invalid agent settings are refused.
	before, _ := b.Workspaces.Get(ctx, tenant, "ws_core")
	got, err := ws.UpdateSettings(ctx, tenant, "ws_core", WorkspaceSettingsPatch{})
	if err != nil || !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("empty patch = %+v, %v", got, err)
	}
	if _, err := ws.UpdateSettings(ctx, tenant, "ws_core", WorkspaceSettingsPatch{
		AgentSettings: &domain.AgentSettingsPatch{DisabledAgents: &[]string{"janitor"}}}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("unknown agent: %v", err)
	}
	if after, _ := b.Workspaces.Get(ctx, tenant, "ws_core"); !slices.Equal(after.AgentSettings.DisabledAgents, before.AgentSettings.DisabledAgents) {
		t.Error("a refused patch changed the settings")
	}
}
