package tools

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/internal/service"
)

// blockerEnv is a blocked task with a blocker job on it, and a registry
// holding the M9 tools backed by a real TaskService on memory repositories.
type blockerEnv struct {
	b     repotest.Backend
	svc   *service.TaskService
	reg   *Registry
	task  *domain.Task
	job   *domain.AgentJob
	scope Scope
}

func newBlockerEnv(t *testing.T, hitl bool) *blockerEnv {
	t.Helper()
	b := repotest.Memory()
	svc := service.NewTaskService(service.Deps{Tx: b.Tx, Tasks: b.Tasks, Workspaces: b.Workspaces, Comments: b.Comments,
		Outbox: b.Outbox, Dependencies: b.Deps, AgentJobs: b.AgentJobs}, service.Config{PublicBaseURL: "https://t"})
	w, _ := domain.NewWorkspace("tenant_acme", "ws", "WS", t0)
	if err := b.Workspaces.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	due := t0.Add(48 * time.Hour)
	task, err := svc.CreateTask(ctx, domain.User("usr_alice"), domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws",
		Title: "Ship the release", AssigneeID: "usr_bob", DueAt: &due})
	if err != nil {
		t.Fatal(err)
	}
	blocked := domain.StatusBlocked
	if task, err = svc.UpdateTask(ctx, domain.User("usr_bob"), "tenant_acme", task.ID, task.Version, domain.Patch{Status: &blocked}); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(b.Tasks)
	for _, tool := range []Tool{DraftResolutionComment{Tasks: svc}, ReassignTask{Tasks: svc},
		ShiftDueDate{Tasks: svc}, ArchiveTask{Tasks: svc}} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	version := task.Version
	job := &domain.AgentJob{ID: uuid.New(), TenantID: "tenant_acme", TaskID: &task.ID, AgentType: domain.AgentBlocker, BaseTaskVersion: &version}
	settings := domain.DefaultAgentSettings()
	settings.RequireHITLForAll = hitl
	return &blockerEnv{b: b, svc: svc, reg: reg, task: task, job: job, scope: NewScope(job, settings, nil)}
}

func (e *blockerEnv) stored(t *testing.T) *domain.Task {
	t.Helper()
	got, err := e.b.Tasks.Get(ctx, "tenant_acme", e.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e *blockerEnv) comments(t *testing.T) []domain.Comment {
	t.Helper()
	got, err := e.b.Comments.ListRecent(ctx, e.task.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e *blockerEnv) outboxTypes(t *testing.T) []string {
	t.Helper()
	rows, err := e.b.Outbox.ListForAggregate(ctx, e.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.EventType)
	}
	return out
}

// approve stores proposals on the job and applies them, as a person's
// approval does.
func (e *blockerEnv) approve(t *testing.T, proposals ...*Proposal) error {
	t.Helper()
	var list []Proposal
	for _, p := range proposals {
		list = append(list, *p)
	}
	e.job.ProposedActions, _ = json.Marshal(list)
	return e.reg.ApplyProposals(ctx, e.job)
}

func TestDraftResolutionCommentIsTier1(t *testing.T) {
	e := newBlockerEnv(t, false)
	outboxBefore := len(e.outboxTypes(t))
	args := map[string]any{"task_id": e.task.ID, "body": "Waiting on the signing key. @usr_carol can issue one today.",
		"reasoning": "The last three comments ask for the key."}
	res, err := e.reg.Dispatch(ctx, e.scope, toolCall(DraftResolutionCommentName, args))
	if err != nil || !res.Executed() || res.Tier != domain.Tier1 {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
	out, ok := res.Output.(CommentResult)
	if !ok || !out.Created || out.CommentID == uuid.Nil {
		t.Fatalf("output = %#v", res.Output)
	}
	if _, versioned := res.Output.(VersionedOutput); versioned {
		t.Error("a comment does not move the version, so the output must not report one")
	}

	cs := e.comments(t)
	if len(cs) != 1 || cs[0].ID != out.CommentID || cs[0].AuthorID != "agent:blocker_investigator" ||
		cs[0].Body != args["body"] || len(cs[0].Mentions) != 1 || cs[0].Mentions[0] != "usr_carol" {
		t.Fatalf("comments = %+v", cs)
	}
	got := e.stored(t)
	if got.Version != e.task.Version || got.Status != domain.StatusBlocked {
		t.Errorf("the comment changed the task: version %d, status %s", got.Version, got.Status)
	}
	runs := got.AgentMetadata.Runs
	if len(runs) != 1 || runs[0].JobID != e.job.ID || runs[0].Agent != domain.AgentBlocker ||
		runs[0].Reasoning != "The last three comments ask for the key." || runs[0].Changes["comment_id"] != out.CommentID.String() {
		t.Errorf("runs = %+v", runs)
	}
	// One row per recipient: the assignee and the mentioned user.
	types := e.outboxTypes(t)
	outboxAfter := len(types)
	if outboxAfter != outboxBefore+2 || types[outboxAfter-1] != domain.EventTaskCommentAdded {
		t.Errorf("outbox = %v", types)
	}
	if jobs, _ := e.b.AgentJobs.ListForTask(ctx, "tenant_acme", e.task.ID); len(jobs) != 0 {
		t.Errorf("an agent comment queued jobs: %+v", jobs)
	}

	// A second call from the same job, as on a retried run, posts nothing.
	res, err = e.reg.Dispatch(ctx, e.scope, toolCall(DraftResolutionCommentName, args))
	if err != nil {
		t.Fatal(err)
	}
	if again := res.Output.(CommentResult); again.Created || again.CommentID != out.CommentID {
		t.Errorf("second call = %+v", again)
	}
	if len(e.comments(t)) != 1 || len(e.outboxTypes(t)) != outboxAfter || len(e.stored(t).AgentMetadata.Runs) != 1 {
		t.Error("the second call wrote something")
	}

	// Another job may comment again.
	other := e.scope
	other.JobID = uuid.New()
	if res, err = e.reg.Dispatch(ctx, other, toolCall(DraftResolutionCommentName, args)); err != nil || !res.Output.(CommentResult).Created {
		t.Errorf("another job = %+v, %v", res, err)
	}
}

func TestDraftResolutionCommentUnderHITL(t *testing.T) {
	e := newBlockerEnv(t, true)
	res, err := e.reg.Dispatch(ctx, e.scope, toolCall(DraftResolutionCommentName, map[string]any{
		"task_id": e.task.ID, "body": "Brief", "reasoning": "r"}))
	if err != nil || res.Executed() {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
	if len(e.comments(t)) != 0 {
		t.Fatal("a proposal posted a comment")
	}
	if err := e.approve(t, res.Proposal); err != nil {
		t.Fatal(err)
	}
	if cs := e.comments(t); len(cs) != 1 || cs[0].AuthorID != "agent:blocker_investigator" {
		t.Errorf("after approval = %+v", cs)
	}
}

func TestDraftResolutionCommentArguments(t *testing.T) {
	e := newBlockerEnv(t, false)
	for name, args := range map[string]map[string]any{
		"no body":      {"task_id": e.task.ID, "reasoning": "r"},
		"empty body":   {"task_id": e.task.ID, "body": "", "reasoning": "r"},
		"no reasoning": {"task_id": e.task.ID, "body": "b"},
		"extra field":  {"task_id": e.task.ID, "body": "b", "reasoning": "r", "author_id": "usr_alice"},
	} {
		_, err := e.reg.Dispatch(ctx, e.scope, toolCall(DraftResolutionCommentName, args))
		wantRejected(t, name, err, CodeInvalidArguments)
	}
	// Whitespace passes the schema but is still not a comment.
	_, err := e.reg.Dispatch(ctx, e.scope, toolCall(DraftResolutionCommentName, map[string]any{
		"task_id": e.task.ID, "body": "   ", "reasoning": "r"}))
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("blank body: %v", err)
	}
	if len(e.comments(t)) != 0 || len(e.stored(t).AgentMetadata.Runs) != 0 {
		t.Error("a refused call wrote something")
	}
}

// tier2Case is one Tier 2 tool: its arguments and how to tell it applied.
type tier2Case struct {
	tool    string
	args    func(e *blockerEnv) map[string]any
	applied func(t *testing.T, got *domain.Task)
	change  string
}

var newDue = time.Date(2026, 11, 30, 17, 0, 0, 0, time.UTC)

var tier2Cases = []tier2Case{
	{
		tool: ReassignTaskName, change: "assignee_id",
		args: func(e *blockerEnv) map[string]any {
			return map[string]any{"task_id": e.task.ID, "assignee_id": "usr_carol", "reasoning": "Carol owns the signing service."}
		},
		applied: func(t *testing.T, got *domain.Task) {
			if got.AssigneeID != "usr_carol" {
				t.Errorf("assignee = %q", got.AssigneeID)
			}
		},
	},
	{
		tool: ShiftDueDateName, change: "due_at",
		args: func(e *blockerEnv) map[string]any {
			return map[string]any{"task_id": e.task.ID, "due_at": "2026-11-30T18:00:00+01:00", "reasoning": "The key arrives Monday."}
		},
		applied: func(t *testing.T, got *domain.Task) {
			if got.DueAt == nil || !got.DueAt.Equal(newDue) || got.LastDueSoonNotifiedAt != nil {
				t.Errorf("due_at = %v, last due-soon = %v", got.DueAt, got.LastDueSoonNotifiedAt)
			}
		},
	},
	{
		tool: ArchiveTaskName, change: "status",
		args: func(e *blockerEnv) map[string]any {
			return map[string]any{"task_id": e.task.ID, "reasoning": "The release was cancelled."}
		},
		applied: func(t *testing.T, got *domain.Task) {
			if got.Status != domain.StatusArchived || got.ArchivedAt == nil || got.BlockedReason != nil {
				t.Errorf("status = %s, archived_at = %v", got.Status, got.ArchivedAt)
			}
		},
	},
}

func TestBlockerTier2Tools(t *testing.T) {
	for _, tc := range tier2Cases {
		t.Run(tc.tool, func(t *testing.T) {
			// Tier 2 even when the workspace does not require approval for
			// everything.
			e := newBlockerEnv(t, false)
			args := tc.args(e)
			res, err := e.reg.Dispatch(ctx, e.scope, toolCall(tc.tool, args))
			if err != nil || res.Executed() || res.Tier != domain.Tier2 || res.Proposal.Tool != tc.tool {
				t.Fatalf("Dispatch = %+v, %v", res, err)
			}
			before := e.stored(t)
			if before.Version != e.task.Version || before.AssigneeID != "usr_bob" || before.Status != domain.StatusBlocked ||
				len(before.AgentMetadata.Runs) != 0 {
				t.Fatalf("a proposal changed the task: %+v", before)
			}

			if err := e.approve(t, res.Proposal); err != nil {
				t.Fatal(err)
			}
			got := e.stored(t)
			tc.applied(t, got)
			if got.Version != e.task.Version+1 {
				t.Errorf("version = %d, want %d", got.Version, e.task.Version+1)
			}
			runs := got.AgentMetadata.Runs
			if len(runs) != 1 || runs[0].JobID != e.job.ID || runs[0].Agent != domain.AgentBlocker ||
				runs[0].Reasoning != args["reasoning"] || runs[0].Changes[tc.change] == nil {
				t.Errorf("runs = %+v", runs)
			}
		})
	}
}

func TestBlockerTier2ToolsAreStaleAfterAnEdit(t *testing.T) {
	for _, tc := range tier2Cases {
		t.Run(tc.tool, func(t *testing.T) {
			e := newBlockerEnv(t, false)
			res, err := e.reg.Dispatch(ctx, e.scope, toolCall(tc.tool, tc.args(e)))
			if err != nil {
				t.Fatal(err)
			}
			title := "Ship the release on Monday"
			if _, err := e.svc.UpdateTask(ctx, domain.User("usr_bob"), "tenant_acme", e.task.ID, e.task.Version,
				domain.Patch{Title: &title}); err != nil {
				t.Fatal(err)
			}
			if err := e.approve(t, res.Proposal); !errors.Is(err, domain.ErrVersionConflict) {
				t.Fatalf("approve after an edit: %v", err)
			}
			got := e.stored(t)
			if got.AssigneeID != "usr_bob" || got.Status != domain.StatusBlocked || len(got.AgentMetadata.Runs) != 0 {
				t.Errorf("a stale proposal applied: %+v", got)
			}
		})
	}
}

func TestBlockerToolArguments(t *testing.T) {
	e := newBlockerEnv(t, false)
	for name, c := range map[string]struct {
		tool string
		args map[string]any
	}{
		"reassign without assignee": {ReassignTaskName, map[string]any{"task_id": e.task.ID, "reasoning": "r"}},
		"reassign to nobody":        {ReassignTaskName, map[string]any{"task_id": e.task.ID, "assignee_id": "", "reasoning": "r"}},
		"due date not a date":       {ShiftDueDateName, map[string]any{"task_id": e.task.ID, "due_at": "next Monday", "reasoning": "r"}},
		"due date without zone":     {ShiftDueDateName, map[string]any{"task_id": e.task.ID, "due_at": "2026-11-30T18:00:00", "reasoning": "r"}},
		"archive without reasoning": {ArchiveTaskName, map[string]any{"task_id": e.task.ID}},
	} {
		_, err := e.reg.Dispatch(ctx, e.scope, toolCall(c.tool, c.args))
		wantRejected(t, name, err, CodeInvalidArguments)
	}
	// A task outside the job's scope is refused before anything is proposed.
	other, err := e.svc.CreateTask(ctx, domain.User("usr_alice"), domain.NewTaskInput{TenantID: "tenant_acme", WorkspaceID: "ws", Title: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.reg.Dispatch(ctx, e.scope, toolCall(ArchiveTaskName, map[string]any{"task_id": other.ID, "reasoning": "r"}))
	wantRejected(t, "another task", err, CodeOutOfScope)
}
