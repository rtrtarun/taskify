package repotest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
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
		claimed, err := b.AgentJobs.Claim(ctx, now, 2, domain.AgentTypes)
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
		if rest, _ := b.AgentJobs.Claim(ctx, now, 5, domain.AgentTypes); len(rest) != 1 || rest[0].ID != ids[0] {
			t.Errorf("second claim = %+v", rest)
		}
		if none, _ := b.AgentJobs.Claim(ctx, now, 5, domain.AgentTypes); len(none) != 0 {
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
					got, err := b.AgentJobs.Claim(ctx, t0, 3, domain.AgentTypes)
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
		claimed, err := b.AgentJobs.Claim(ctx, t0, 1, domain.AgentTypes)
		if err != nil || len(claimed) != 1 {
			t.Fatal(claimed, err)
		}
		j := claimed[0]

		if _, err := b.AgentJobs.GetForUpdate(ctx, tenant, j.ID); !errors.Is(err, repository.ErrTxRequired) {
			t.Errorf("GetForUpdate outside a transaction: %v", err)
		}

		leased := j.UpdatedAt // the claim's lease
		if !leased.Equal(t0) {
			t.Fatalf("claimed at %v", leased)
		}

		// A heartbeat keeps the status and moves the lease.
		beat := *j
		beat.UpdatedAt = t0.Add(30 * time.Second)
		if err := b.AgentJobs.Update(ctx, &beat, domain.JobRunning, leased); err != nil {
			t.Fatal(err)
		}
		// The old lease no longer matches, although the status does: a
		// worker whose job was recovered and claimed again loses.
		if err := b.AgentJobs.Update(ctx, j, domain.JobRunning, leased); !errors.Is(err, repository.ErrJobMoved) {
			t.Errorf("update with an old lease: %v", err)
		}
		leased = beat.UpdatedAt

		at := t0.Add(time.Minute)
		if err := j.TransitionTo(domain.JobAwaitingApproval, at); err != nil {
			t.Fatal(err)
		}
		j.ProposedActions = json.RawMessage(`[{"tool": "UpdateTaskAttributes", "args": {"priority": "high"}}]`)
		j.Attempts = 1
		j.BaseTaskVersion = ptr(3)
		if err := b.AgentJobs.Update(ctx, j, domain.JobRunning, leased); err != nil {
			t.Fatal(err)
		}

		// A second writer that still thinks the job is running loses.
		stale := *j
		stale.Status = domain.JobFailed
		if err := b.AgentJobs.Update(ctx, &stale, domain.JobRunning, leased); !errors.Is(err, repository.ErrJobMoved) {
			t.Errorf("update from a stale status: %v", err)
		}

		err = b.Tx.WithTx(ctx, func(ctx context.Context) error {
			locked, err := b.AgentJobs.GetForUpdate(ctx, tenant, j.ID)
			if err != nil {
				return err
			}
			decided, prev := at.Add(time.Minute), locked.UpdatedAt
			if err := locked.TransitionTo(domain.JobCompleted, decided); err != nil {
				return err
			}
			locked.DecidedBy, locked.DecidedAt = "usr_bob", &decided
			locked.ExecutionResult = json.RawMessage(`{"applied": 1}`)
			return b.AgentJobs.Update(ctx, locked, domain.JobAwaitingApproval, prev)
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.AgentJobs.Get(ctx, tenant, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.JobCompleted || got.Attempts != 1 || got.DecidedBy != "usr_bob" || *got.BaseTaskVersion != 3 ||
			!got.DecidedAt.Equal(at.Add(time.Minute)) || !got.UpdatedAt.Equal(at.Add(time.Minute)) ||
			!jsonEq(got.ProposedActions, j.ProposedActions) || !jsonEq(got.ExecutionResult, json.RawMessage(`{"applied":1}`)) {
			t.Errorf("updated job = %+v", got)
		}

		// A decision needs both who and when, as the schema requires.
		half := *got
		half.DecidedAt = nil
		if err := b.AgentJobs.Update(ctx, &half, domain.JobCompleted, got.UpdatedAt); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("half a decision: %v", err)
		}

		gone := *got
		gone.ID = uuid.New()
		if err := b.AgentJobs.Update(ctx, &gone, domain.JobCompleted, got.UpdatedAt); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("update of a missing job: %v", err)
		}
		other := *got
		other.TenantID = "tenant_other"
		if err := b.AgentJobs.Update(ctx, &other, domain.JobCompleted, got.UpdatedAt); !errors.Is(err, domain.ErrNotFound) {
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

func TestAgentJobClaimByType(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		triage := newJob(t, nil, domain.AgentTriage, t0)
		blocker := newJob(t, nil, domain.AgentBlocker, t0.Add(-time.Minute)) // older
		insertJob(t, b, triage)
		insertJob(t, b, blocker)
		if none, err := b.AgentJobs.Claim(ctx, t0, 5, nil); err != nil || len(none) != 0 {
			t.Errorf("claim with no agent types = %+v, %v", none, err)
		}
		got, err := b.AgentJobs.Claim(ctx, t0, 5, []string{domain.AgentTriage})
		if err != nil || len(got) != 1 || got[0].ID != triage.ID {
			t.Fatalf("claim triage only = %+v, %v", got, err)
		}
		// The blocker job waits for a runtime that has that agent.
		if j, _ := b.AgentJobs.Get(ctx, tenant, blocker.ID); j.Status != domain.JobPending {
			t.Errorf("blocker job = %s", j.Status)
		}
	})
}

func TestAgentJobRecoverStuck(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		jobs := map[string]domain.AgentJob{}
		for _, name := range []string{"stuck", "stuck-last-try", "fresh", "pending"} {
			j := newJob(t, nil, domain.AgentTriage, t0)
			insertJob(t, b, j)
			jobs[name] = j
		}
		// Claim three at different times; "pending" stays pending.
		claimAt := map[string]time.Time{"stuck": t0, "stuck-last-try": t0.Add(time.Second), "fresh": t0.Add(10 * time.Minute)}
		for _, name := range []string{"stuck", "stuck-last-try", "fresh"} {
			j := jobs[name]
			got, err := b.AgentJobs.Get(ctx, tenant, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			running, prev := *got, got.UpdatedAt
			running.Status, running.UpdatedAt = domain.JobRunning, claimAt[name]
			if name == "stuck-last-try" {
				running.Attempts = 2
			}
			// Pending -> running directly, as Claim does.
			if err := b.AgentJobs.Update(ctx, &running, domain.JobPending, prev); err != nil {
				t.Fatal(err)
			}
		}

		now := t0.Add(11 * time.Minute)
		requeued, failed, err := b.AgentJobs.RecoverStuck(ctx, now.Add(-5*time.Minute), now, 3)
		if err != nil || requeued != 1 || failed != 1 {
			t.Fatalf("RecoverStuck = %d requeued, %d failed, %v", requeued, failed, err)
		}
		check := func(name string, status domain.JobStatus, attempts int) {
			t.Helper()
			got, _ := b.AgentJobs.Get(ctx, tenant, jobs[name].ID)
			if got.Status != status || got.Attempts != attempts {
				t.Errorf("%s = %s with %d attempts, want %s with %d", name, got.Status, got.Attempts, status, attempts)
			}
			if status == domain.JobFailed && got.ErrorMessage == "" {
				t.Errorf("%s failed without a message", name)
			}
		}
		check("stuck", domain.JobPending, 1)
		check("stuck-last-try", domain.JobFailed, 3)
		check("fresh", domain.JobRunning, 0)
		check("pending", domain.JobPending, 0)
	})
}

func TestAgentJobRetryAfter(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		j := newJob(t, nil, domain.AgentTriage, t0)
		insertJob(t, b, j)
		claimed, err := b.AgentJobs.Claim(ctx, t0, 1, domain.AgentTypes)
		if err != nil || len(claimed) != 1 {
			t.Fatal(claimed, err)
		}
		got := claimed[0]
		if got.RetryAfter != nil || got.ConflictCount != 0 {
			t.Errorf("new job: retry_after %v, conflicts %d", got.RetryAfter, got.ConflictCount)
		}

		// Back to pending, held until a minute from now.
		lease, retry := got.UpdatedAt, t0.Add(time.Minute)
		if err := got.TransitionTo(domain.JobPending, t0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		got.RetryAfter, got.ConflictCount = &retry, 2
		if err := b.AgentJobs.Update(ctx, got, domain.JobRunning, lease); err != nil {
			t.Fatal(err)
		}
		stored, _ := b.AgentJobs.Get(ctx, tenant, j.ID)
		if stored.RetryAfter == nil || !stored.RetryAfter.Equal(retry) || stored.ConflictCount != 2 {
			t.Errorf("stored = retry_after %v, conflicts %d", stored.RetryAfter, stored.ConflictCount)
		}
		if early, _ := b.AgentJobs.Claim(ctx, retry.Add(-time.Millisecond), 1, domain.AgentTypes); len(early) != 0 {
			t.Error("a job was claimed before its retry_after")
		}
		if due, _ := b.AgentJobs.Claim(ctx, retry, 1, domain.AgentTypes); len(due) != 1 {
			t.Error("a job was not claimed at its retry_after")
		}

		bad := *stored
		bad.ConflictCount = -1
		if err := b.AgentJobs.Update(ctx, &bad, domain.JobRunning, retry); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("negative conflict count: %v", err)
		}
	})
}

func TestTaskEstimateAndAgentMetadata(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		task := newTask(t, tenant, nil)
		task.EstimatedMinutes = ptr(45)
		create(t, b, task)
		got, err := b.Tasks.Get(ctx, tenant, task.ID)
		if err != nil || got.EstimatedMinutes == nil || *got.EstimatedMinutes != 45 ||
			got.AgentMetadata.Runs != nil || got.AgentMetadata.PriorityRecommendation != nil {
			t.Fatalf("created = %+v, %v", got, err)
		}

		job := uuid.New()
		got.RecordAgentRun(domain.AgentRunRecord{JobID: job, Agent: domain.AgentTriage, At: t0,
			Changes: map[string]any{"labels": []any{"bug"}, "estimated_minutes": float64(90)}, Reasoning: "crash in checkout"})
		if err := got.RecommendPriority(domain.PriorityRecommendation{Value: domain.PriorityHigh, JobID: job, At: t0}); err != nil {
			t.Fatal(err)
		}
		got.EstimatedMinutes = ptr(90)
		got.Version++
		if err := b.Tasks.Update(ctx, got, got.Version-1); err != nil {
			t.Fatal(err)
		}
		again, _ := b.Tasks.Get(ctx, tenant, task.ID)
		if *again.EstimatedMinutes != 90 || len(again.AgentMetadata.Runs) != 1 {
			t.Fatalf("updated = %+v", again)
		}
		run := again.AgentMetadata.Runs[0]
		if run.JobID != job || run.Reasoning != "crash in checkout" || !run.At.Equal(t0) || run.Changes["estimated_minutes"] != float64(90) {
			t.Errorf("run = %+v", run)
		}
		if rec := again.AgentMetadata.PriorityRecommendation; rec == nil || rec.Value != domain.PriorityHigh || rec.JobID != job {
			t.Errorf("recommendation = %+v", rec)
		}

		// A stored copy is not changed through the caller's value.
		again.AgentMetadata.Runs[0].Changes["labels"] = "mutated"
		if fresh, _ := b.Tasks.Get(ctx, tenant, task.ID); fresh.AgentMetadata.Runs[0].Changes["labels"] == "mutated" {
			t.Error("the store shares agent metadata with callers")
		}
	})
}

func TestTriageQueries(t *testing.T) {
	each(t, func(t *testing.T, b Backend) {
		seedWorkspace(t, b, tenant)
		other, _ := domain.NewWorkspace(tenant, "ws_other", "Other", t0)
		if err := b.Workspaces.Create(ctx, other); err != nil {
			t.Fatal(err)
		}
		mk := func(title, workspace string, labels []string, status domain.Status) *domain.Task {
			t.Helper()
			task := newTask(t, tenant, func(in *domain.NewTaskInput) { in.Title, in.WorkspaceID, in.Labels = title, workspace, labels })
			create(t, b, task)
			if status != domain.StatusTodo {
				if _, err := task.TransitionTo(status, alice, t0); err != nil {
					t.Fatal(err)
				}
				if err := b.Tasks.Update(ctx, task, task.Version-1); err != nil {
					t.Fatal(err)
				}
			}
			return task
		}
		target := mk("Checkout page crashes on empty cart", ws, []string{"bug"}, domain.StatusTodo)
		strong := mk("Checkout crashes when the cart is empty", ws, []string{"bug", "checkout"}, domain.StatusInProgress)
		weak := mk("Cart icon misaligned", ws, []string{"ui", "bug"}, domain.StatusTodo)
		mk("Update the logo", ws, []string{"ui"}, domain.StatusTodo)
		mk("Checkout crashes on empty cart, fixed", ws, nil, domain.StatusCompleted)               // closed
		mk("Checkout crashes on empty cart", "ws_other", []string{"elsewhere"}, domain.StatusTodo) // other workspace

		got, err := b.Tasks.SimilarOpen(ctx, tenant, ws, target.ID, target.Title, 5)
		if err != nil {
			t.Fatal(err)
		}
		var ids []uuid.UUID
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		if len(ids) != 2 || ids[0] != strong.ID || ids[1] != weak.ID {
			t.Errorf("similar = %v, want [strong %s, weak %s]", ids, strong.ID, weak.ID)
		}
		if one, _ := b.Tasks.SimilarOpen(ctx, tenant, ws, target.ID, target.Title, 1); len(one) != 1 || one[0].ID != strong.ID {
			t.Errorf("limit 1 = %v", one)
		}
		if none, err := b.Tasks.SimilarOpen(ctx, tenant, ws, target.ID, "a of", 5); err != nil || len(none) != 0 {
			t.Errorf("no terms = %v, %v", none, err)
		}
		if none, _ := b.Tasks.SimilarOpen(ctx, "tenant_other", ws, uuid.Nil, target.Title, 5); len(none) != 0 {
			t.Errorf("another tenant sees %d tasks", len(none))
		}

		labels, err := b.Tasks.WorkspaceLabels(ctx, tenant, ws, 10)
		if err != nil || !slices.Equal(labels, []string{"bug", "ui", "checkout"}) {
			t.Errorf("labels = %v, %v", labels, err)
		}
		if two, _ := b.Tasks.WorkspaceLabels(ctx, tenant, ws, 2); !slices.Equal(two, []string{"bug", "ui"}) {
			t.Errorf("labels limit 2 = %v", two)
		}
	})
}
