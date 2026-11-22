package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// faultCallbacks fails the next calls of the named callback methods.
type faultCallbacks struct {
	repository.WebhookDeliveryRepository
	mu   sync.Mutex
	fail map[string]int
}

func (f *faultCallbacks) hit(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[method] == 0 {
		return nil
	}
	f.fail[method]--
	return errInjected
}

func (f *faultCallbacks) RecordCallback(ctx context.Context, tenantID, callbackID string, at time.Time) (bool, error) {
	if err := f.hit("RecordCallback"); err != nil {
		return false, err
	}
	return f.WebhookDeliveryRepository.RecordCallback(ctx, tenantID, callbackID, at)
}

func (f *faultCallbacks) CallbackSeen(ctx context.Context, tenantID, callbackID string) (bool, error) {
	if err := f.hit("CallbackSeen"); err != nil {
		return false, err
	}
	return f.WebhookDeliveryRepository.CallbackSeen(ctx, tenantID, callbackID)
}

// syncBuffer is a log destination safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestWebhookCallbackRecordFailures (AP-06): an action's callbackId is
// recorded in the transaction that applies it, so when the record fails the
// action is not applied and AgentPulse's retry applies it once. A decision
// commits on its own first; when its callbackId cannot be checked it is not
// made, and when it cannot be recorded afterwards the decision stands and
// is logged, and a retry changes nothing.
func TestWebhookCallbackRecordFailures(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		jf := newJobsFixture(t, f)
		sender := &recordingSender{}
		deliveries := &faultCallbacks{WebhookDeliveryRepository: f.b.Deliveries, fail: map[string]int{}}
		logs := &syncBuffer{}
		hooks := NewWebhookService(f.b.Tx, deliveries, f.svc, NewFeedbackService(f.b.Tasks, sender, nil),
			slog.New(slog.NewTextHandler(logs, nil)), nil).WithAgentJobs(jf.jobs)
		task := f.create(t, nil)

		// An action whose callbackId cannot be recorded.
		complete := Callback{Key: "sig-1", CallbackID: callbackID, Action: ActionComplete, TenantID: tenant, UserID: bob.ID, TaskID: task.ID}
		deliveries.fail["RecordCallback"] = 1
		if _, err := hooks.Handle(ctx, complete); !errors.Is(err, errInjected) {
			t.Fatalf("Handle = %v, want the injected failure", err)
		}
		if got := f.get(t, task.ID); got.Status != domain.StatusTodo || got.Version != task.Version {
			t.Errorf("a failed record still applied the action: %s v%d", got.Status, got.Version)
		}
		if len(sender.signals()) != 0 {
			t.Errorf("feedback for an action not applied: %v", sender.signals())
		}
		complete.Key = "sig-2"
		if dup, err := hooks.Handle(ctx, complete); err != nil || dup {
			t.Fatalf("retry = %v, %v", dup, err)
		}
		if got := f.get(t, task.ID); got.Status != domain.StatusCompleted {
			t.Errorf("retry left the task %s", got.Status)
		}

		// A decision whose callbackId cannot be checked is not made.
		other := f.create(t, nil)
		job := jf.proposal(t, other, "from Slack")
		approve := Callback{Key: "sig-3", CallbackID: "cb-" + strings.Repeat("ab", 16), Action: ActionApprove,
			TenantID: tenant, UserID: bob.ID, JobID: job.ID}
		deliveries.fail["CallbackSeen"] = 1
		if _, err := hooks.Handle(ctx, approve); !errors.Is(err, errInjected) {
			t.Fatalf("Handle = %v, want the injected failure", err)
		}
		if got := jf.job(t, job.ID); got.Status != domain.JobAwaitingApproval {
			t.Errorf("job = %s, want still awaiting approval", got.Status)
		}

		// A decision made but not recorded stands, once.
		deliveries.fail["RecordCallback"] = 1
		if dup, err := hooks.Handle(ctx, approve); err != nil || dup {
			t.Fatalf("approve = %v, %v", dup, err)
		}
		if got := jf.job(t, job.ID); got.Status != domain.JobCompleted || f.get(t, other.ID).Description != "from Slack" {
			t.Errorf("job = %s, task description %q", got.Status, f.get(t, other.ID).Description)
		}
		if !strings.Contains(logs.String(), "decision applied but delivery not recorded") {
			t.Errorf("the lost record was not logged:\n%s", logs)
		}
		if seen, _ := f.b.Deliveries.CallbackSeen(ctx, tenant, approve.CallbackID); seen {
			t.Error("the callbackId was recorded although the record failed")
		}
		approve.Key = "sig-4"
		if dup, err := hooks.Handle(ctx, approve); err != nil || !dup {
			t.Errorf("retry = %v, %v; want a duplicate", dup, err)
		}
		if got := sender.signals(); len(got) != 2 {
			t.Errorf("feedback = %v, want one signal per applied click", got)
		}
	})
}

// TestWebhookRefusesUnprintableUserIDs (AP-11): a callback's userId must be
// one Taskify could have issued, whatever the action.
func TestWebhookRefusesUnprintableUserIDs(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		hooks := NewWebhookService(f.b.Tx, f.b.Deliveries, f.svc, NewFeedbackService(f.b.Tasks, &recordingSender{}, nil), quiet, nil)
		task := f.create(t, nil)
		for _, user := range []string{"usr_bob\n", "usr_böb", strings.Repeat("u", domain.MaxUserIDLen+1)} {
			for _, action := range []string{ActionComplete, ActionApprove} {
				cb := Callback{Key: "sig-" + uuid.NewString(), Action: action, TenantID: tenant, UserID: user, TaskID: task.ID, JobID: uuid.New()}
				if _, err := hooks.Handle(ctx, cb); !errors.Is(err, domain.ErrValidation) {
					t.Errorf("%s by %q: %v", action, user, err)
				}
			}
		}
		if got := f.get(t, task.ID); got.Status != domain.StatusTodo {
			t.Errorf("a refused callback changed the task: %s", got.Status)
		}
	})
}
