package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type integration struct {
	*fixture
	mock       *mockagentpulse.Server
	client     *agentpulse.Client
	workspaces *WorkspaceService
	feedback   *FeedbackService
	webhooks   *WebhookService
}

func eachIntegration(t *testing.T, fn func(t *testing.T, it *integration)) {
	t.Helper()
	for _, factory := range repotest.Factories(t) {
		t.Run(factory.Name, func(t *testing.T) {
			t.Parallel()
			b := factory.New(t)
			f := newFixture(t, b, nil)
			mock := mockagentpulse.New()
			ts := httptest.NewServer(mock)
			t.Cleanup(ts.Close)
			client := agentpulse.New(ts.URL, "k", agentpulse.WithBreaker(nil))
			fb := NewFeedbackService(b.Tasks, client, nil)
			it := &integration{
				fixture:    f,
				mock:       mock,
				client:     client,
				workspaces: NewWorkspaceService(b.Tx, b.Workspaces, b.Rules, client, func() time.Time { return f.now }),
				feedback:   fb,
				webhooks:   NewWebhookService(b.Tx, b.Deliveries, f.svc, fb, quiet, func() time.Time { return f.now }),
			}
			fn(t, it)
		})
	}
}

func TestWorkspaceCreateBootstrapsRules(t *testing.T) {
	eachIntegration(t, func(t *testing.T, it *integration) {
		w, rules, rulesErr, err := it.workspaces.Create(ctx, tenant, "ws_new", "New")
		if err != nil || rulesErr != nil {
			t.Fatalf("Create = %v, rules error %v", err, rulesErr)
		}
		if w.Name != "New" || len(rules) != 3 {
			t.Fatalf("workspace %+v, %d rules", w, len(rules))
		}
		sent := it.mock.Rules()
		if len(sent) != 3 {
			t.Fatalf("AgentPulse received %d rules, want 3", len(sent))
		}
		// The exact bodies (AP-03). The critical rule makes p1_critical tasks,
		// manual blocks, and escalations critical, each entry with a real
		// condition besides the workspace.
		want := []string{
			`{"tenantId":"tenant_acme","name":"taskify:ws_new:critical-or-blocked","attentionClass":"critical","matchAny":[` +
				`{"payloadMatch":{"priority":"p1_critical","workspaceId":"ws_new"}},` +
				`{"payloadMatch":{"blockedReason":"manual","status":"blocked","workspaceId":"ws_new"}},` +
				`{"eventTypes":["task.escalated"],"payloadMatch":{"workspaceId":"ws_new"}}],` +
				`"channels":["slack","email","in_app"],"bypassDigest":true}`,
			`{"tenantId":"tenant_acme","name":"taskify:ws_new:assignments-and-mentions","matchAny":[` +
				`{"eventTypes":["task.assigned"],"payloadMatch":{"workspaceId":"ws_new"}},` +
				`{"eventTypes":["task.comment_added"],"payloadMatch":{"isMentioned":true,"workspaceId":"ws_new"}}],` +
				`"channels":["in_app","email"]}`,
			`{"tenantId":"tenant_acme","name":"taskify:ws_new:low-priority-digest","matchAny":[` +
				`{"payloadMatch":{"priority":"p4_low","workspaceId":"ws_new"}}],"channels":["digest"]}`,
		}
		reqs := it.mock.RequestsTo("POST", "/v1/rules")
		if len(reqs) != len(want) {
			t.Fatalf("POST /v1/rules requests = %d, want %d", len(reqs), len(want))
		}
		for i, r := range reqs {
			if string(r.Body) != want[i] {
				t.Errorf("rule %d body:\n got %s\nwant %s", i, r.Body, want[i])
			}
		}

		// Bootstrapping again changes nothing.
		again, err := it.workspaces.BootstrapRules(ctx, tenant, "ws_new")
		if err != nil || len(again) != 3 || len(it.mock.Rules()) != 3 {
			t.Errorf("second bootstrap: %d rules, %d sent, %v", len(again), len(it.mock.Rules()), err)
		}
		if _, err := it.workspaces.BootstrapRules(ctx, tenant, "ws_missing"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("missing workspace: error = %v", err)
		}
		if _, _, _, err := it.workspaces.Create(ctx, tenant, "ws_new", "Dup"); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Errorf("duplicate: error = %v", err)
		}
	})
}

func TestWorkspaceBootstrapSurvivesPartialFailure(t *testing.T) {
	eachIntegration(t, func(t *testing.T, it *integration) {
		// The second rule fails: the workspace still exists and the first rule is kept.
		calls := 0
		failing := ruleCreatorFunc(func(ctx context.Context, r agentpulse.Rule) (string, error) {
			calls++
			if calls == 2 {
				return "", &agentpulse.StatusError{Code: 503}
			}
			return it.client.CreateRule(ctx, r)
		})
		ws := NewWorkspaceService(it.b.Tx, it.b.Workspaces, it.b.Rules, failing, nil)
		w, rules, rulesErr, err := ws.Create(ctx, tenant, "ws_flaky", "Flaky")
		if err != nil || w.ID != "ws_flaky" {
			t.Fatalf("Create = %+v, %v", w, err)
		}
		if rulesErr == nil || !agentpulse.IsUpstream(rulesErr) || rules != nil {
			t.Fatalf("rules error = %v, rules %v", rulesErr, rules)
		}
		if recorded, _ := it.b.Rules.List(ctx, tenant, "ws_flaky"); len(recorded) != 1 {
			t.Fatalf("recorded %d rules after the failure, want 1", len(recorded))
		}
		// A retry fills in the rest without repeating the first.
		rules, err = ws.BootstrapRules(ctx, tenant, "ws_flaky")
		if err != nil || len(rules) != 3 || len(it.mock.Rules()) != 3 {
			t.Errorf("retry: %d rules, %d sent to AgentPulse, %v", len(rules), len(it.mock.Rules()), err)
		}
	})
}

func TestWorkspaceBootstrapConcurrent(t *testing.T) {
	eachIntegration(t, func(t *testing.T, it *integration) {
		if _, err := it.svc.CreateWorkspace(ctx, tenant, "ws_race", "Race"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := it.workspaces.BootstrapRules(ctx, tenant, "ws_race"); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if n := len(it.mock.Rules()); n != 3 {
			t.Errorf("AgentPulse received %d rules from 4 concurrent bootstraps, want 3", n)
		}
	})
}

type ruleCreatorFunc func(context.Context, agentpulse.Rule) (string, error)

func (f ruleCreatorFunc) CreateRule(ctx context.Context, r agentpulse.Rule) (string, error) {
	return f(ctx, r)
}

func TestWorkspaceRename(t *testing.T) {
	eachIntegration(t, func(t *testing.T, it *integration) {
		it.now = t0.Add(time.Hour)
		w, err := it.workspaces.Rename(ctx, tenant, "ws_core", "  Core Platform ")
		if err != nil || w.Name != "Core Platform" || !w.UpdatedAt.Equal(it.now) {
			t.Fatalf("Rename = %+v, %v", w, err)
		}
		if got, _ := it.workspaces.Get(ctx, tenant, "ws_core"); got.Name != "Core Platform" {
			t.Errorf("stored name = %q", got.Name)
		}
		if _, err := it.workspaces.Rename(ctx, tenant, "ws_core", " "); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("empty name: error = %v", err)
		}
		if _, err := it.workspaces.Rename(ctx, "tenant_other", "ws_core", "x"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("other tenant: error = %v", err)
		}
	})
}

func TestFeedback(t *testing.T) {
	eachIntegration(t, func(t *testing.T, it *integration) {
		task := it.create(t, nil)
		in := FeedbackInput{TenantID: tenant, UserID: bob.ID, TaskID: task.ID, NotificationID: "ntf_1", Signal: SignalSnoozed, Duration: "2h"}
		if err := it.feedback.Record(ctx, in); err != nil {
			t.Fatal(err)
		}
		sent := it.mock.Feedback()
		if len(sent) != 1 || sent[0]["signal"] != "snoozed" || sent[0]["snoozeDuration"] != "2h" ||
			sent[0]["taskId"] != task.ID.String() || sent[0]["userId"] != bob.ID || sent[0]["tenantId"] != tenant {
			t.Fatalf("sent = %v", sent)
		}
		if stored, _ := it.b.Tasks.Get(ctx, tenant, task.ID); stored.Version != 1 {
			t.Error("feedback changed the task")
		}

		for name, bad := range map[string]FeedbackInput{
			"unknown signal":          {Signal: "shrug"},
			"snooze without duration": {Signal: SignalSnoozed},
			"snooze too long":         {Signal: SignalSnoozed, Duration: "200h"},
			"negative snooze":         {Signal: SignalSnoozed, Duration: "-1h"},
			"duration on acted":       {Signal: SignalActed, Duration: "1h"},
		} {
			bad.TenantID, bad.UserID, bad.TaskID = tenant, bob.ID, task.ID
			if err := it.feedback.Record(ctx, bad); !errors.Is(err, domain.ErrValidation) {
				t.Errorf("%s: error = %v", name, err)
			}
		}
		other := in
		other.TenantID = "tenant_other"
		if err := it.feedback.Record(ctx, other); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("another tenant's task: error = %v", err)
		}
		it.mock.FailNext("POST", "/v1/feedback", http.StatusServiceUnavailable, 1)
		in.Signal, in.Duration = SignalActed, ""
		if err := it.feedback.Record(ctx, in); !agentpulse.IsUpstream(err) {
			t.Errorf("AgentPulse down: error = %v", err)
		}
	})
}

func TestWebhookComplete(t *testing.T) {
	eachIntegration(t, func(t *testing.T, it *integration) {
		task := it.create(t, nil)
		cb := Callback{Key: "sig-1", Action: ActionComplete, TenantID: tenant, UserID: bob.ID, TaskID: task.ID, NotificationID: "ntf_9"}

		dup, err := it.webhooks.Handle(ctx, cb)
		if err != nil || dup {
			t.Fatalf("Handle = %v, %v", dup, err)
		}
		stored, _ := it.b.Tasks.Get(ctx, tenant, task.ID)
		if stored.Status != domain.StatusCompleted {
			t.Errorf("status = %s", stored.Status)
		}
		if fb := it.mock.Feedback(); len(fb) != 1 || fb[0]["signal"] != "acted" || fb[0]["notificationId"] != "ntf_9" {
			t.Errorf("feedback = %v", fb)
		}

		// A replay is acknowledged and changes nothing.
		dup, err = it.webhooks.Handle(ctx, cb)
		if err != nil || !dup {
			t.Errorf("replay = %v, %v", dup, err)
		}
		if again, _ := it.b.Tasks.Get(ctx, tenant, task.ID); again.Version != stored.Version {
			t.Error("a replay changed the task")
		}
		if len(it.mock.Feedback()) != 1 {
			t.Error("a replay forwarded feedback again")
		}
	})
}

func TestWebhookUnblock(t *testing.T) {
	eachIntegration(t, func(t *testing.T, it *integration) {
		// A manual block and a dependency block are both lifted.
		manual := it.create(t, nil)
		it.setStatus(t, manual, domain.StatusBlocked)
		dependent := it.create(t, nil)
		it.link(t, dependent, it.unassigned(t, "Open blocker"))
		for i, task := range []*domain.Task{manual, dependent} {
			before := it.get(t, task.ID)
			if before.Status != domain.StatusBlocked {
				t.Fatalf("setup: %s", before.Status)
			}
			cb := Callback{Key: "sig-u" + strconv.Itoa(i), Action: ActionUnblock, TenantID: tenant, UserID: "usr_dave", TaskID: task.ID, NotificationID: "ntf_u"}
			if dup, err := it.webhooks.Handle(ctx, cb); err != nil || dup {
				t.Fatalf("Handle = %v, %v", dup, err)
			}
			after := it.get(t, task.ID)
			if after.Status != domain.StatusInProgress || after.BlockedReason != nil || after.Version != before.Version+1 {
				t.Errorf("after unblock: %s %v v%d", after.Status, after.BlockedReason, after.Version)
			}
			// Repeated with a new signature: already in progress, no change.
			cb.Key += "-again"
			if _, err := it.webhooks.Handle(ctx, cb); err != nil {
				t.Errorf("second click: %v", err)
			}
			if again := it.get(t, task.ID); again.Version != after.Version {
				t.Error("a second click changed the task")
			}
		}
		if fb := it.mock.Feedback(); len(fb) != 4 || fb[0]["signal"] != "acted" || fb[0]["userId"] != "usr_dave" {
			t.Errorf("feedback = %v", fb)
		}
		// A todo task is not blocked: refused, and nothing is recorded.
		todo := it.create(t, nil)
		cb := Callback{Key: "sig-todo", Action: ActionUnblock, TenantID: tenant, UserID: "usr_dave", TaskID: todo.ID}
		if _, err := it.webhooks.Handle(ctx, cb); !errors.Is(err, domain.ErrInvalidStateTransition) {
			t.Errorf("unblock a todo task: %v", err)
		}
	})
}

func TestWebhookSnooze(t *testing.T) {
	eachIntegration(t, func(t *testing.T, it *integration) {
		task := it.create(t, nil)
		before := len(it.outbox(t, task.ID))
		it.now = t0.Add(time.Hour)
		_, err := it.webhooks.Handle(ctx, Callback{Key: "sig-s", Action: ActionSnooze, TenantID: tenant, UserID: bob.ID, TaskID: task.ID, Duration: "2h"})
		if err != nil {
			t.Fatal(err)
		}
		stored, _ := it.b.Tasks.Get(ctx, tenant, task.ID)
		if stored.RemindAt == nil || !stored.RemindAt.Equal(it.now.Add(2*time.Hour)) {
			t.Errorf("RemindAt = %v", stored.RemindAt)
		}
		if after := len(it.outbox(t, task.ID)); after != before {
			t.Errorf("a snooze wrote %d notifications", after-before)
		}
		if fb := it.mock.Feedback(); len(fb) != 1 || fb[0]["signal"] != "snoozed" || fb[0]["snoozeDuration"] != "2h" {
			t.Errorf("feedback = %v", fb)
		}
	})
}

func TestWebhookFailuresAndValidation(t *testing.T) {
	eachIntegration(t, func(t *testing.T, it *integration) {
		task := it.create(t, nil)

		// The task is in another tenant: nothing is recorded, so a corrected
		// delivery with the same key is still processed.
		wrong := Callback{Key: "sig-x", Action: ActionComplete, TenantID: "tenant_other", UserID: bob.ID, TaskID: task.ID}
		if _, err := it.webhooks.Handle(ctx, wrong); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
		if fresh, _ := it.b.Deliveries.Record(ctx, "sig-x", t0); !fresh {
			t.Error("a failed callback was recorded as delivered")
		}

		// Feedback failing does not undo or fail the action.
		it.mock.FailNext("POST", "/v1/feedback", http.StatusServiceUnavailable, 1)
		dup, err := it.webhooks.Handle(ctx, Callback{Key: "sig-y", Action: ActionComplete, TenantID: tenant, UserID: bob.ID, TaskID: task.ID})
		if err != nil || dup {
			t.Fatalf("Handle with AgentPulse feedback down = %v, %v", dup, err)
		}
		if stored, _ := it.b.Tasks.Get(ctx, tenant, task.ID); stored.Status != domain.StatusCompleted {
			t.Error("the action was undone by a feedback failure")
		}

		for name, cb := range map[string]Callback{
			"unknown action":   {Action: "explode"},
			"missing tenant":   {Action: ActionComplete, TenantID: " "},
			"missing task":     {Action: ActionComplete, TaskID: uuid.Nil},
			"snooze no length": {Action: ActionSnooze},
		} {
			if cb.TenantID == "" {
				cb.TenantID = tenant
			}
			if cb.TaskID == uuid.Nil && name != "missing task" {
				cb.TaskID = task.ID
			}
			cb.UserID, cb.Key = bob.ID, "sig-"+name
			if _, err := it.webhooks.Handle(ctx, cb); !errors.Is(err, domain.ErrValidation) {
				t.Errorf("%s: error = %v", name, err)
			}
		}
	})
}

func TestDeleteTask(t *testing.T) {
	eachIntegration(t, func(t *testing.T, it *integration) {
		task := it.create(t, nil)
		if _, err := it.svc.AddComment(ctx, alice, tenant, task.ID, "note"); err != nil {
			t.Fatal(err)
		}
		// A relay worker holds one row while the task is deleted.
		claimed, err := it.b.Outbox.ClaimBatch(ctx, t0, 1)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim = %d, %v", len(claimed), err)
		}

		if err := it.svc.DeleteTask(ctx, "tenant_other", task.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("another tenant: error = %v", err)
		}
		if err := it.svc.DeleteTask(ctx, tenant, task.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := it.b.Tasks.Get(ctx, tenant, task.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Error("the task survived")
		}
		if rows, _ := it.b.Outbox.ListForAggregate(ctx, task.ID); len(rows) != 0 {
			t.Errorf("%d outbox rows survived", len(rows))
		}
		if comments, _ := it.b.Comments.ListRecent(ctx, task.ID, 10); len(comments) != 0 {
			t.Error("comments survived")
		}
		// The worker finishing afterwards is not an error.
		if err := it.b.Outbox.MarkDispatched(ctx, claimed[0].ID, *claimed[0].ClaimedAt, t0); err != nil {
			t.Errorf("late result after delete: %v", err)
		}
		if err := it.svc.DeleteTask(ctx, tenant, task.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("second delete: error = %v", err)
		}
	})
}
