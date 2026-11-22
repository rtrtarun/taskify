package agent

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/agent/tools"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

// blockerRuntime runs the real blocker investigator with its four real tools.
func (f *fixture) blockerRuntime(t *testing.T) *Runtime {
	t.Helper()
	for _, tool := range []tools.Tool{
		tools.DraftResolutionComment{Tasks: f.svc}, tools.LinkDependencies{Tasks: f.svc},
		tools.ReassignTask{Tasks: f.svc}, tools.ShiftDueDate{Tasks: f.svc},
	} {
		if err := f.reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	r, err := New(Deps{Tx: f.b.Tx, Outbox: f.b.Outbox, Jobs: f.b.AgentJobs, Tasks: f.b.Tasks, Workspaces: f.b.Workspaces, LLM: f.mock, Tools: f.reg},
		Config{Logger: quiet, Now: f.clock.now, PublicBaseURL: "https://taskify.test", Workers: 1},
		f.blocker())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) blocker() Blocker {
	return Blocker{Tasks: f.b.Tasks, Comments: f.b.Comments, Dependencies: f.b.Deps}
}

// owned creates a task as creator, assigned to assignee ("" for none).
func (f *fixture) owned(t *testing.T, creator, assignee, title string) *domain.Task {
	t.Helper()
	task, err := f.svc.CreateTask(ctx, domain.User(creator), domain.NewTaskInput{
		TenantID: "tenant_acme", WorkspaceID: "ws", Title: title, AssigneeID: assignee})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *fixture) setStatus(t *testing.T, actor domain.Actor, id uuid.UUID, s domain.Status) *domain.Task {
	t.Helper()
	cur := f.svcTask(t, id)
	task, err := f.svc.UpdateTask(ctx, actor, "tenant_acme", id, cur.Version, domain.Patch{Status: &s})
	if err != nil {
		t.Fatalf("set %s: %v", s, err)
	}
	return task
}

func (f *fixture) link(t *testing.T, task, blocker *domain.Task) {
	t.Helper()
	if _, err := f.svc.AddDependency(ctx, "tenant_acme", task.ID, blocker.ID); err != nil {
		t.Fatal(err)
	}
}

// blockerJobs returns the task's blocker investigations.
func (f *fixture) blockerJobs(t *testing.T, id uuid.UUID) []*domain.AgentJob {
	t.Helper()
	jobs, err := f.b.AgentJobs.ListForTask(ctx, "tenant_acme", id)
	if err != nil {
		t.Fatal(err)
	}
	var out []*domain.AgentJob
	for _, j := range jobs {
		if j.AgentType == domain.AgentBlocker {
			out = append(out, j)
		}
	}
	return out
}

// escalations returns the task.escalated notifications for a task, by
// recipient.
func (f *fixture) escalations(t *testing.T, id uuid.UUID) map[string]domain.Notification {
	t.Helper()
	entries, err := f.b.Outbox.ListForAggregate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]domain.Notification{}
	for _, e := range entries {
		if e.EventType != domain.EventTaskEscalated {
			continue
		}
		var n domain.Notification
		if err := json.Unmarshal(e.Payload, &n); err != nil {
			t.Fatal(err)
		}
		if _, dup := out[n.UserID]; dup {
			t.Errorf("%s escalated twice", n.UserID)
		}
		out[n.UserID] = n
	}
	return out
}

func blockingIDs(n domain.Notification) []string {
	var ids []string
	list, _ := n.Payload["blockingTasks"].([]any)
	for _, b := range list {
		ids = append(ids, b.(map[string]any)["taskId"].(string))
	}
	slices.Sort(ids)
	return ids
}

func brief(id uuid.UUID) map[string]any {
	return map[string]any{"task_id": id, "body": "Waiting on the vendor key from Carol and the DNS change from Dave.",
		"reasoning": "Both open blockers are named in the comments."}
}

// manuallyBlocked builds a task assigned to bob that depends on two open
// tasks of carol's, an unassigned open task created by dave, and a
// completed task of erin's, and that alice then marks blocked.
func (f *fixture) manuallyBlocked(t *testing.T) (blocked *domain.Task, carol1, carol2, dave, erin *domain.Task) {
	t.Helper()
	blocked = f.owned(t, "usr_alice", "usr_bob", "Launch the partner integration")
	carol1 = f.owned(t, "usr_alice", "usr_carol", "Get the vendor key")
	carol2 = f.owned(t, "usr_alice", "usr_carol", "Sign the vendor contract")
	dave = f.owned(t, "usr_dave", "", "Change the DNS records")
	erin = f.owned(t, "usr_alice", "usr_erin", "Write the spec")
	f.setStatus(t, alice, blocked.ID, domain.StatusInProgress)
	for _, b := range []*domain.Task{carol1, carol2, dave, erin} {
		f.link(t, blocked, b) // in progress: the status is left alone
	}
	f.setStatus(t, domain.User("usr_erin"), erin.ID, domain.StatusCompleted)
	if _, err := f.svc.AddComment(ctx, domain.User("usr_bob"), "tenant_acme", blocked.ID, "Still no vendor key."); err != nil {
		t.Fatal(err)
	}
	blocked = f.setStatus(t, alice, blocked.ID, domain.StatusBlocked)
	return blocked, carol1, carol2, dave, erin
}

// TestBlockerBriefAndEscalation covers two M9 exit criteria: the brief is a
// comment authored by the agent, and the escalation goes to the owners of
// the blocking tasks, not to the blocked task's assignee.
func TestBlockerBriefAndEscalation(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		blocked, carol1, carol2, dave, _ := f.manuallyBlocked(t)
		jobs := f.blockerJobs(t, blocked.ID)
		if len(jobs) != 1 || jobs[0].DedupKey != domain.EventJobDedupKey(domain.AgentBlocker, blocked.ID, blocked.Version) {
			t.Fatalf("a manual block queued %v", jobs)
		}
		job := jobs[0]
		f.mock.Then(llm.Call("c1", tools.DraftResolutionCommentName, brief(blocked.ID)), llm.Text("Two people can clear this."))
		if n, err := f.blockerRuntime(t).RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("RunOnce = %d, %v", n, err)
		}
		if got := f.job(t, job.ID); got.Status != domain.JobCompleted {
			t.Fatalf("job = %s (%s)", got.Status, got.ErrorMessage)
		}

		comments, err := b.Comments.ListRecent(ctx, blocked.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(comments) < 2 || comments[1].AuthorID != "usr_bob" || comments[0].AuthorID != domain.Agent(domain.AgentBlocker).ID ||
			!strings.Contains(comments[0].Body, "vendor key") {
			t.Fatalf("comments = %+v", comments)
		}
		stored := f.svcTask(t, blocked.ID)
		if stored.Status != domain.StatusBlocked || stored.Version != blocked.Version {
			t.Errorf("the brief changed the task: %s v%d", stored.Status, stored.Version)
		}
		if id, ok := stored.CommentOf(job.ID); !ok || id != comments[0].ID {
			t.Errorf("agent_metadata does not record the brief: %v %v", id, ok)
		}

		got := f.escalations(t, blocked.ID)
		if len(got) != 2 {
			t.Fatalf("escalated to %d people, want carol and dave: %v", len(got), got)
		}
		if _, ok := got["usr_bob"]; ok {
			t.Error("the blocked task's assignee was escalated to")
		}
		if _, ok := got["usr_erin"]; ok {
			t.Error("the owner of a completed blocker was escalated to")
		}
		carol, ok := got["usr_carol"]
		want := []string{carol1.ID.String(), carol2.ID.String()}
		slices.Sort(want)
		if !ok || !slices.Equal(blockingIDs(carol), want) {
			t.Errorf("carol's escalation = %+v", carol)
		}
		if d, ok := got["usr_dave"]; !ok || !slices.Equal(blockingIDs(d), []string{dave.ID.String()}) {
			t.Errorf("dave (creator of an unassigned blocker) = %+v", d)
		}
		urls, _ := carol.Payload["actionUrls"].(map[string]any)
		labels, _ := carol.Payload["actionLabels"].(map[string]any)
		if carol.IdempotencyKey != "taskify:tenant_acme:"+blocked.ID.String()+":task.escalated:"+job.ID.String()+":usr_carol" ||
			urls[domain.ActionUnblock] == nil || labels[domain.ActionUnblock] != "Unblock and set In Progress" ||
			carol.Payload["jobId"] != job.ID.String() || carol.Payload["taskId"] != blocked.ID.String() ||
			carol.Payload["bypassDigest"] != true || carol.PriorityHint != "high" {
			t.Errorf("escalation = %+v", carol)
		}

		// Escalating again for the same job writes nothing.
		events, err := f.blocker().FinishEvents(ctx, f.job(t, job.ID), f.clock.now())
		if err != nil || len(events) != 2 {
			t.Fatalf("FinishEvents = %v, %v", events, err)
		}
		entries, err := domain.NewOutboxEntries(events, "https://taskify.test", 10, f.clock.now())
		if err != nil {
			t.Fatal(err)
		}
		if n, err := b.Outbox.Insert(ctx, entries); err != nil || n != 0 {
			t.Errorf("a repeated escalation wrote %d rows, %v", n, err)
		}
	})
}

// TestBlockerWithoutOpenBlockerOnlyComments: a manual block with no open
// dependency, and a stale task, get the brief and no escalation.
func TestBlockerWithoutOpenBlockerOnlyComments(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		manual := f.owned(t, "usr_alice", "usr_bob", "Waiting on legal")
		done := f.owned(t, "usr_alice", "usr_carol", "Draft the terms")
		f.setStatus(t, alice, manual.ID, domain.StatusInProgress)
		f.link(t, manual, done)
		f.setStatus(t, domain.User("usr_carol"), done.ID, domain.StatusCompleted)
		f.setStatus(t, alice, manual.ID, domain.StatusBlocked)

		stale := f.owned(t, "usr_alice", "usr_bob", "Quiet work")
		f.setStatus(t, alice, stale.ID, domain.StatusInProgress)
		staleJob, err := domain.NewStaleJob(f.svcTask(t, stale.ID), f.clock.now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.AgentJobs.Insert(ctx, staleJob); err != nil {
			t.Fatal(err)
		}

		r := f.blockerRuntime(t)
		for _, task := range []*domain.Task{manual, stale} {
			jobs := f.blockerJobs(t, task.ID)
			if len(jobs) != 1 {
				t.Fatalf("%s has %d blocker jobs", task.Title, len(jobs))
			}
			f.mock.Then(llm.Call("c1", tools.DraftResolutionCommentName, brief(task.ID)), llm.Text("Asked the assignee."))
			if n, err := r.RunOnce(ctx); err != nil || n != 1 {
				t.Fatalf("RunOnce = %d, %v", n, err)
			}
			if got := f.job(t, jobs[0].ID); got.Status != domain.JobCompleted {
				t.Fatalf("%s job = %s (%s)", task.Title, got.Status, got.ErrorMessage)
			}
			if _, ok := f.svcTask(t, task.ID).CommentOf(jobs[0].ID); !ok {
				t.Errorf("%s got no brief", task.Title)
			}
			if got := f.escalations(t, task.ID); len(got) != 0 {
				t.Errorf("%s was escalated: %v", task.Title, got)
			}
		}
		// The stale run was told why it runs.
		reqs := f.mock.Requests()
		if first := reqs[len(reqs)-2].Messages[1].Content; !strings.Contains(first, "no comment for two days") ||
			!strings.Contains(first, `"trigger": "stale"`) {
			t.Errorf("stale context = %s", first)
		}
	})
}

// TestStaleTaskWithOpenBlockerIsEscalatedWithoutUnblock: a stale task that
// waits on an open dependency escalates to its owner, but offers no unblock
// action, since the task is not blocked.
func TestStaleTaskWithOpenBlockerIsEscalatedWithoutUnblock(t *testing.T) {
	f := newFixture(t, repotest.Memory(), false)
	task := f.owned(t, "usr_alice", "usr_bob", "Migrate the billing data")
	dep := f.owned(t, "usr_alice", "usr_carol", "Freeze the schema")
	f.setStatus(t, alice, task.ID, domain.StatusInProgress)
	f.link(t, task, dep)
	job, err := domain.NewStaleJob(f.svcTask(t, task.ID), f.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.AgentJobs.Insert(ctx, job); err != nil {
		t.Fatal(err)
	}
	f.mock.Then(llm.Call("c1", tools.DraftResolutionCommentName, brief(task.ID)), llm.Text("Waiting on the schema freeze."))
	if _, err := f.blockerRuntime(t).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := f.escalations(t, task.ID)
	carol, ok := got["usr_carol"]
	if len(got) != 1 || !ok {
		t.Fatalf("escalations = %v", got)
	}
	urls, _ := carol.Payload["actionUrls"].(map[string]any)
	if urls[domain.ActionUnblock] != nil || carol.Payload["actionLabels"] != nil || urls["view"] == nil {
		t.Errorf("an in-progress task offered unblock: %+v", carol.Payload)
	}
}

// TestBlockerProposalsStillEscalate: with approval required for everything,
// the brief waits for approval, and the escalation goes out with the
// approval request when the run ends.
func TestBlockerProposalsStillEscalate(t *testing.T) {
	f := newFixture(t, repotest.Memory(), true)
	blocked, _, _, _, _ := f.manuallyBlocked(t)
	job := f.blockerJobs(t, blocked.ID)[0]
	f.mock.Then(
		llm.Call("c1", tools.DraftResolutionCommentName, brief(blocked.ID)),
		llm.Call("c2", tools.ReassignTaskName, map[string]any{"task_id": blocked.ID, "assignee_id": "usr_carol", "reasoning": "Carol holds both blockers."}),
		llm.Text("Proposed a brief and a reassignment."))
	if _, err := f.blockerRuntime(t).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.job(t, job.ID); got.Status != domain.JobAwaitingApproval {
		t.Fatalf("job = %s (%s)", got.Status, got.ErrorMessage)
	}
	if _, ok := f.svcTask(t, blocked.ID).CommentOf(job.ID); ok {
		t.Error("a proposed brief was posted")
	}
	if got := f.escalations(t, blocked.ID); len(got) != 2 {
		t.Errorf("escalations = %v", got)
	}
	entries, _ := f.b.Outbox.ListForAggregate(ctx, blocked.ID)
	approvals := 0
	for _, e := range entries {
		if e.EventType == domain.EventAgentApprovalRequested {
			approvals++
		}
	}
	if approvals != 1 {
		t.Errorf("%d approval requests", approvals)
	}
}

// TestFailedBlockerRunDoesNotEscalate: escalation belongs to a run that
// ended well.
func TestFailedBlockerRunDoesNotEscalate(t *testing.T) {
	f := newFixture(t, repotest.Memory(), false)
	blocked, _, _, _, _ := f.manuallyBlocked(t)
	f.mock.Then(llm.Call("c1", "NoSuchTool", map[string]any{}), llm.Call("c2", "NoSuchTool", map[string]any{}))
	if _, err := f.blockerRuntime(t).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.job(t, f.blockerJobs(t, blocked.ID)[0].ID); got.Status != domain.JobFailed {
		t.Fatalf("job = %s", got.Status)
	}
	if got := f.escalations(t, blocked.ID); len(got) != 0 {
		t.Errorf("a failed run escalated: %v", got)
	}
}

// TestDependencyBlockStartsNoBlockerJob is the M9-03 test "a dependency
// block starts no job"; a person's manual block starts one.
func TestDependencyBlockStartsNoBlockerJob(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		f := newFixture(t, b, false)
		task := f.owned(t, "usr_alice", "usr_bob", "Deploy")
		dep := f.owned(t, "usr_alice", "usr_carol", "Build")
		f.link(t, task, dep)
		got := f.svcTask(t, task.ID)
		if got.Status != domain.StatusBlocked || *got.BlockedReason != domain.BlockedDependency {
			t.Fatalf("task = %s %v", got.Status, got.BlockedReason)
		}
		if jobs := f.blockerJobs(t, task.ID); len(jobs) != 0 {
			t.Errorf("a dependency block queued %d blocker jobs", len(jobs))
		}
		other := f.owned(t, "usr_alice", "usr_bob", "Review")
		f.setStatus(t, domain.User("usr_bob"), other.ID, domain.StatusBlocked)
		if jobs := f.blockerJobs(t, other.ID); len(jobs) != 1 {
			t.Errorf("a manual block queued %d blocker jobs", len(jobs))
		}
	})
}

func TestBlockerContext(t *testing.T) {
	f := newFixture(t, repotest.Memory(), false)
	blocked, carol1, _, dave, erin := f.manuallyBlocked(t)
	similar := f.owned(t, "usr_alice", "usr_frank", "Launch the partner portal")
	a := f.blocker()
	if a.Type() != domain.AgentBlocker || !slices.Equal(a.AllowedTools(), []string{
		tools.DraftResolutionCommentName, tools.LinkDependenciesName, tools.ReassignTaskName, tools.ShiftDueDateName}) {
		t.Fatalf("blocker = %s %v", a.Type(), a.AllowedTools())
	}
	job := f.blockerJobs(t, blocked.ID)[0]
	msgs, err := a.BuildContext(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || !strings.Contains(msgs[0].Content, "data written by users") || !strings.Contains(msgs[1].Content, "was marked blocked") {
		t.Fatalf("messages = %+v", msgs)
	}
	var c blockerContextJSON
	if err := json.Unmarshal([]byte(msgs[1].Content[strings.Index(msgs[1].Content, "{"):]), &c); err != nil {
		t.Fatal(err)
	}
	if c.Trigger != domain.EventTaskBlocked || c.Task.ID != blocked.ID.String() || c.Task.BlockedReason != "manual" ||
		c.Task.AssigneeID != "usr_bob" || len(c.RecentComments) == 0 || c.RecentComments[0].Author != "usr_bob" || len(c.BlockingTasks) != 4 {
		t.Fatalf("context = %+v", c)
	}
	owners := map[string]blockerDependencyJSON{}
	for _, d := range c.BlockingTasks {
		owners[d.ID] = d
	}
	if d := owners[carol1.ID.String()]; d.Owner != "usr_carol" || !d.Open {
		t.Errorf("carol's blocker = %+v", d)
	}
	if d := owners[dave.ID.String()]; d.Owner != "usr_dave" || !d.Open {
		t.Errorf("dave's blocker = %+v", d)
	}
	if d := owners[erin.ID.String()]; d.Open || d.Status != "completed" {
		t.Errorf("erin's blocker = %+v", d)
	}
	if len(c.SimilarOpenTasks) != 1 || c.SimilarOpenTasks[0].ID != similar.ID.String() {
		t.Errorf("similar = %+v", c.SimilarOpenTasks)
	}
	if _, err := a.BuildContext(ctx, &domain.AgentJob{TenantID: "tenant_acme"}); err == nil {
		t.Error("a job without a task was accepted")
	}
}
