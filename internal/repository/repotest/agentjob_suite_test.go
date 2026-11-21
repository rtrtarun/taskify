package repotest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

func newJob(t *testing.T, task *domain.Task, agent string, at time.Time) domain.AgentJob {
	t.Helper()
	in := domain.NewAgentJobInput{TenantID: tenant, AgentType: agent, InputContext: json.RawMessage(`{"trigger": "task.updated"}`)}
	if task != nil {
		in.TaskID, in.BaseTaskVersion = &task.ID, &task.Version
		in.DedupKey = domain.EventJobDedupKey(agent, task.ID, task.Version)
	} else {
		in.DedupKey = agent + ":" + uuid.NewString()
	}
	j, err := domain.NewAgentJob(in, at)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func insertJob(t *testing.T, b Backend, j domain.AgentJob) {
	t.Helper()
	if created, err := b.AgentJobs.Insert(ctx, j); err != nil || !created {
		t.Fatalf("insert job: %v, %v", created, err)
	}
}

// jsonEq compares JSON by value: PostgreSQL's jsonb does not keep spacing.
func jsonEq(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func TestAgentJobInsertAndDedup(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		task := newTask(t, tenant, nil)
		create(t, b, task)

		j := newJob(t, task, domain.AgentTriage, t0)
		insertJob(t, b, j)
		got, err := b.AgentJobs.Get(ctx, tenant, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.JobPending || got.AgentType != domain.AgentTriage || *got.TaskID != task.ID ||
			*got.BaseTaskVersion != 1 || got.DedupKey != j.DedupKey || !got.CreatedAt.Equal(t0) ||
			!jsonEq(got.InputContext, j.InputContext) || got.ProposedActions != nil || got.DecidedAt != nil {
			t.Errorf("stored job = %+v", got)
		}

		// The same dedup key again is not an error and changes nothing.
		again := newJob(t, task, domain.AgentTriage, t0.Add(time.Minute))
		again.InputContext = json.RawMessage(`{"trigger": "other"}`)
		created, err := b.AgentJobs.Insert(ctx, again)
		if err != nil || created {
			t.Fatalf("duplicate insert = %v, %v", created, err)
		}
		if jobs, _ := b.AgentJobs.ListForTask(ctx, tenant, task.ID); len(jobs) != 1 || jobs[0].ID != j.ID {
			t.Errorf("jobs after a duplicate = %+v", jobs)
		}

		// A duplicate inside a transaction leaves the transaction usable.
		err = b.Tx.WithTx(ctx, func(ctx context.Context) error {
			if _, err := b.AgentJobs.Insert(ctx, again); err != nil {
				return err
			}
			_, err := b.AgentJobs.Insert(ctx, newJob(t, task, domain.AgentBlocker, t0))
			return err
		})
		if err != nil {
			t.Fatalf("transaction with a duplicate: %v", err)
		}

		// A job with no task, such as a morning digest.
		insertJob(t, b, newJob(t, nil, domain.AgentDigest, t0))

		if _, err := b.AgentJobs.Get(ctx, "tenant_other", j.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Get from another tenant: %v", err)
		}
		if jobs, _ := b.AgentJobs.ListForTask(ctx, "tenant_other", task.ID); len(jobs) != 0 {
			t.Errorf("ListForTask from another tenant = %+v", jobs)
		}
		missing := newJob(t, newTask(t, tenant, nil), domain.AgentTriage, t0)
		if _, err := b.AgentJobs.Insert(ctx, missing); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("job for a missing task: %v", err)
		}
	})
}

func TestAgentJobClaim(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		var ids []uuid.UUID
		for i := range 3 {
			j := newJob(t, nil, domain.AgentTriage, t0.Add(time.Duration(2-i)*time.Minute)) // newest first
			insertJob(t, b, j)
			ids = append(ids, j.ID)
		}
		waiting := newJob(t, nil, domain.AgentTriage, t0.Add(-time.Hour))
		waiting.Status = domain.JobAwaitingApproval
		insertJob(t, b, waiting)

		now := t0.Add(time.Hour)
		claimed, err := b.AgentJobs.Claim(ctx, now, 2)
		if err != nil || len(claimed) != 2 {
			t.Fatalf("Claim = %d jobs, %v", len(claimed), err)
		}
		got := map[uuid.UUID]bool{claimed[0].ID: true, claimed[1].ID: true}
		if !got[ids[2]] || !got[ids[1]] {
			t.Errorf("claimed %v, want the two oldest pending jobs", got)
		}
		for _, j := range claimed {
			if j.Status != domain.JobRunning || !j.UpdatedAt.Equal(now) {
				t.Errorf("claimed job = %s at %v", j.Status, j.UpdatedAt)
			}
		}
		if rest, _ := b.AgentJobs.Claim(ctx, now, 5); len(rest) != 1 || rest[0].ID != ids[0] {
			t.Errorf("second claim = %+v", rest)
		}
		if none, _ := b.AgentJobs.Claim(ctx, now, 5); len(none) != 0 {
			t.Errorf("a job awaiting approval or running was claimed: %+v", none)
		}
	})
}

func TestAgentJobClaimRace(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		const jobs, workers = 30, 5
		for i := range jobs {
			insertJob(t, b, newJob(t, nil, domain.AgentTriage, t0.Add(time.Duration(i)*time.Second)))
		}
		var (
			mu    sync.Mutex
			seen  = map[uuid.UUID]int{}
			wg    sync.WaitGroup
			first error
		)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					got, err := b.AgentJobs.Claim(ctx, t0, 3)
					mu.Lock()
					if err != nil && first == nil {
						first = err
					}
					for _, j := range got {
						seen[j.ID]++
					}
					mu.Unlock()
					if err != nil || len(got) == 0 {
						return
					}
				}
			}()
		}
		wg.Wait()
		if first != nil {
			t.Fatal(first)
		}
		if len(seen) != jobs {
			t.Errorf("%d jobs claimed, want %d", len(seen), jobs)
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("job %s claimed %d times", id, n)
			}
		}
	})
}

func TestAgentJobUpdate(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		task := newTask(t, tenant, nil)
		create(t, b, task)
		insertJob(t, b, newJob(t, task, domain.AgentTriage, t0))
		claimed, err := b.AgentJobs.Claim(ctx, t0, 1)
		if err != nil || len(claimed) != 1 {
			t.Fatal(claimed, err)
		}
		j := claimed[0]

		if _, err := b.AgentJobs.GetForUpdate(ctx, tenant, j.ID); !errors.Is(err, repository.ErrTxRequired) {
			t.Errorf("GetForUpdate outside a transaction: %v", err)
		}

		at := t0.Add(time.Minute)
		if err := j.TransitionTo(domain.JobAwaitingApproval, at); err != nil {
			t.Fatal(err)
		}
		j.ProposedActions = json.RawMessage(`[{"tool": "UpdateTaskAttributes", "args": {"priority": "high"}}]`)
		j.Attempts = 1
		if err := b.AgentJobs.Update(ctx, j, domain.JobRunning); err != nil {
			t.Fatal(err)
		}

		// A second writer that still thinks the job is running loses.
		stale := *j
		stale.Status = domain.JobFailed
		if err := b.AgentJobs.Update(ctx, &stale, domain.JobRunning); !errors.Is(err, repository.ErrJobMoved) {
			t.Errorf("update from a stale status: %v", err)
		}

		err = b.Tx.WithTx(ctx, func(ctx context.Context) error {
			locked, err := b.AgentJobs.GetForUpdate(ctx, tenant, j.ID)
			if err != nil {
				return err
			}
			decided := at.Add(time.Minute)
			if err := locked.TransitionTo(domain.JobCompleted, decided); err != nil {
				return err
			}
			locked.DecidedBy, locked.DecidedAt = "usr_bob", &decided
			locked.ExecutionResult = json.RawMessage(`{"applied": 1}`)
			return b.AgentJobs.Update(ctx, locked, domain.JobAwaitingApproval)
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.AgentJobs.Get(ctx, tenant, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.JobCompleted || got.Attempts != 1 || got.DecidedBy != "usr_bob" ||
			!got.DecidedAt.Equal(at.Add(time.Minute)) || !got.UpdatedAt.Equal(at.Add(time.Minute)) ||
			!jsonEq(got.ProposedActions, j.ProposedActions) || !jsonEq(got.ExecutionResult, json.RawMessage(`{"applied":1}`)) {
			t.Errorf("updated job = %+v", got)
		}

		// A decision needs both who and when, as the schema requires.
		half := *got
		half.DecidedAt = nil
		if err := b.AgentJobs.Update(ctx, &half, domain.JobCompleted); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("half a decision: %v", err)
		}

		gone := *got
		gone.ID = uuid.New()
		if err := b.AgentJobs.Update(ctx, &gone, domain.JobCompleted); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("update of a missing job: %v", err)
		}
		other := *got
		other.TenantID = "tenant_other"
		if err := b.AgentJobs.Update(ctx, &other, domain.JobCompleted); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("update from another tenant: %v", err)
		}
	})
}

func TestAgentRunsAndCascade(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		task := newTask(t, tenant, nil)
		create(t, b, task)
		j := newJob(t, task, domain.AgentTriage, t0)
		insertJob(t, b, j)

		for _, turn := range []int{2, 1} {
			run := domain.AgentRun{ID: uuid.New(), JobID: j.ID, Turn: turn, Model: "mock", Outcome: "tool_call",
				PromptTokens: 100 * turn, CompletionTokens: 10, LatencyMS: 40, CreatedAt: t0}
			if turn == 1 {
				run.ToolName = "UpdateTaskAttributes"
				run.ToolArgs = json.RawMessage(`{"task_id": "x"}`)
				run.ToolResult = json.RawMessage(`{"ok": true}`)
			}
			if err := b.AgentJobs.InsertRun(ctx, run); err != nil {
				t.Fatal(err)
			}
		}
		runs, err := b.AgentJobs.ListRuns(ctx, j.ID)
		if err != nil || len(runs) != 2 {
			t.Fatalf("ListRuns = %+v, %v", runs, err)
		}
		r1, r2 := runs[0], runs[1]
		if r1.Turn != 1 || r1.ToolName != "UpdateTaskAttributes" || !jsonEq(r1.ToolArgs, json.RawMessage(`{"task_id":"x"}`)) ||
			r1.PromptTokens != 100 || r1.LatencyMS != 40 || !r1.CreatedAt.Equal(t0) {
			t.Errorf("turn 1 = %+v", r1)
		}
		if r2.Turn != 2 || r2.ToolName != "" || r2.ToolArgs != nil || r2.ToolResult != nil {
			t.Errorf("turn 2 = %+v", r2)
		}
		if err := b.AgentJobs.InsertRun(ctx, domain.AgentRun{ID: uuid.New(), JobID: j.ID, Turn: 0, Outcome: "x", CreatedAt: t0}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("turn 0: %v", err)
		}
		if err := b.AgentJobs.InsertRun(ctx, domain.AgentRun{ID: uuid.New(), JobID: uuid.New(), Turn: 1, Outcome: "x", CreatedAt: t0}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("run for a missing job: %v", err)
		}
		if none, err := b.AgentJobs.ListRuns(ctx, uuid.New()); err != nil || none == nil || len(none) != 0 {
			t.Errorf("runs of an unknown job = %#v, %v", none, err)
		}

		// A permanent delete takes the task's jobs and their history with it.
		if err := b.Tasks.Delete(ctx, tenant, task.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := b.AgentJobs.Get(ctx, tenant, j.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("job after its task was deleted: %v", err)
		}
		if runs, _ := b.AgentJobs.ListRuns(ctx, j.ID); len(runs) != 0 {
			t.Errorf("runs after the task was deleted: %+v", runs)
		}
	})
}

func TestTaskParent(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		parent := newTask(t, tenant, nil)
		create(t, b, parent)
		child := newTask(t, tenant, nil)
		child.ParentID = &parent.ID
		create(t, b, child)

		got, err := b.Tasks.Get(ctx, tenant, child.ID)
		if err != nil || got.ParentID == nil || *got.ParentID != parent.ID {
			t.Fatalf("child = %+v, %v", got, err)
		}
		if p, _ := b.Tasks.Get(ctx, tenant, parent.ID); p.ParentID != nil {
			t.Errorf("parent has a parent: %v", p.ParentID)
		}

		// An update never moves a task to another parent.
		other := uuid.New()
		got.ParentID, got.Title = &other, "Renamed"
		got.Version++
		if err := b.Tasks.Update(ctx, got, got.Version-1); err != nil {
			t.Fatal(err)
		}
		if again, _ := b.Tasks.Get(ctx, tenant, child.ID); again.ParentID == nil || *again.ParentID != parent.ID {
			t.Errorf("parent after update = %v", again.ParentID)
		}

		orphan := newTask(t, tenant, nil)
		missing := uuid.New()
		orphan.ParentID = &missing
		if err := b.Tasks.Create(ctx, orphan); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("missing parent: %v", err)
		}

		// Deleting the parent keeps the child and clears its link.
		if err := b.Tasks.Delete(ctx, tenant, parent.ID); err != nil {
			t.Fatal(err)
		}
		if after, err := b.Tasks.Get(ctx, tenant, child.ID); err != nil || after.ParentID != nil {
			t.Errorf("child after the parent was deleted = %+v, %v", after, err)
		}
	})
}
