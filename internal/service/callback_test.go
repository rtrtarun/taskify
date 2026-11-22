package service

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
)

const callbackID = "cb-0123456789abcdef0123456789abcdef"

// TestWebhookCallbackIDs is AP-06: AgentPulse retries one click with the
// same callbackId and a new signature, and the retry changes nothing and
// forwards no second feedback.
func TestWebhookCallbackIDs(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		sender := &recordingSender{}
		clock := f.now
		hooks := NewWebhookService(f.b.Tx, f.b.Deliveries, f.svc, NewFeedbackService(f.b.Tasks, sender, nil), quiet,
			func() time.Time { return clock })

		// A snooze retried later would otherwise move the reminder again.
		task := f.create(t, nil)
		cb := Callback{Key: "sig-1", CallbackID: callbackID, Action: ActionSnooze, TenantID: tenant, UserID: bob.ID,
			TaskID: task.ID, Duration: "2h"}
		if dup, err := hooks.Handle(ctx, cb); err != nil || dup {
			t.Fatalf("first delivery = %v, %v", dup, err)
		}
		first := f.get(t, task.ID)
		f.now = f.now.Add(10 * time.Minute)
		clock = f.now
		cb.Key = "sig-2" // signed again with a new timestamp
		if dup, err := hooks.Handle(ctx, cb); err != nil || !dup {
			t.Errorf("retry = %v, %v", dup, err)
		}
		if again := f.get(t, task.ID); again.Version != first.Version || !again.RemindAt.Equal(*first.RemindAt) {
			t.Errorf("a retry snoozed again: %v -> %v", first.RemindAt, again.RemindAt)
		}
		if got := sender.signals(); len(got) != 1 {
			t.Errorf("feedback = %v, want one signal", got)
		}

		// The same callbackId in another tenant is another click.
		other := Callback{Key: "sig-3", CallbackID: callbackID, Action: ActionComplete, TenantID: "tenant_other", UserID: bob.ID, TaskID: task.ID}
		if _, err := hooks.Handle(ctx, other); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("another tenant: %v", err)
		}

		// A malformed callbackId is refused before anything happens.
		for _, bad := range []string{"cb-123", "CB-0123456789abcdef0123456789abcdef", "cb-0123456789ABCDEF0123456789ABCDEF", " " + callbackID} {
			c := Callback{Key: "sig-bad", CallbackID: bad, Action: ActionComplete, TenantID: tenant, UserID: bob.ID, TaskID: task.ID}
			if _, err := hooks.Handle(ctx, c); !errors.Is(err, domain.ErrValidation) {
				t.Errorf("callbackId %q: %v", bad, err)
			}
		}
		if got := f.get(t, task.ID); got.Status != domain.StatusTodo {
			t.Errorf("a refused callback changed the task: %s", got.Status)
		}

		// Without a callbackId the signature still decides (D10).
		plain := Callback{Key: "sig-plain", Action: ActionComplete, TenantID: tenant, UserID: bob.ID, TaskID: task.ID}
		if dup, err := hooks.Handle(ctx, plain); err != nil || dup {
			t.Fatalf("plain = %v, %v", dup, err)
		}
		if dup, _ := hooks.Handle(ctx, plain); !dup {
			t.Error("a byte-identical replay without a callbackId was applied")
		}
	})
}

// TestWebhookCallbackRetriesRace: deliveries of one click arriving at once,
// as on two replicas, apply it once and forward one feedback signal.
func TestWebhookCallbackRetriesRace(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		sender := &recordingSender{}
		hooks := NewWebhookService(f.b.Tx, f.b.Deliveries, f.svc, NewFeedbackService(f.b.Tasks, sender, nil), quiet, nil)
		task := f.create(t, nil)
		const deliveries = 6
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			applied int
		)
		for i := 0; i < deliveries; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				dup, err := hooks.Handle(ctx, Callback{Key: "sig-" + strconv.Itoa(i), CallbackID: callbackID, Action: ActionComplete,
					TenantID: tenant, UserID: bob.ID, TaskID: task.ID})
				if err != nil {
					t.Error(err)
					return
				}
				if !dup {
					mu.Lock()
					applied++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		if applied != 1 {
			t.Errorf("applied %d times, want 1", applied)
		}
		if got := sender.signals(); len(got) != 1 {
			t.Errorf("feedback = %v, want one signal", got)
		}
		if got := f.get(t, task.ID); got.Status != domain.StatusCompleted {
			t.Errorf("status = %s", got.Status)
		}
	})
}

// TestWebhookDecisionCallbackIDs: an approval retried with the same
// callbackId is a duplicate and sends no second signal, even concurrently.
func TestWebhookDecisionCallbackIDs(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		jf := newJobsFixture(t, f)
		sender := &recordingSender{}
		hooks := NewWebhookService(f.b.Tx, f.b.Deliveries, f.svc, NewFeedbackService(f.b.Tasks, sender, nil), quiet, nil).
			WithAgentJobs(jf.jobs)
		task := f.create(t, nil)
		job := jf.proposal(t, task, "from Slack")

		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			applied int
		)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				dup, err := hooks.Handle(ctx, Callback{Key: "sig-" + strconv.Itoa(i), CallbackID: callbackID, Action: ActionApprove,
					TenantID: tenant, UserID: bob.ID, JobID: job.ID})
				if err != nil {
					t.Error(err)
					return
				}
				if !dup {
					mu.Lock()
					applied++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		if applied != 1 || len(sender.signals()) != 1 {
			t.Errorf("applied %d, feedback %v; want 1 and one signal", applied, sender.signals())
		}
		// Recorded: a later retry is a duplicate without touching the job.
		if seen, _ := f.b.Deliveries.CallbackSeen(ctx, tenant, callbackID); !seen {
			t.Error("the callbackId was not recorded")
		}
		dup, err := hooks.Handle(ctx, Callback{Key: "sig-late", CallbackID: callbackID, Action: ActionReject,
			TenantID: tenant, UserID: bob.ID, JobID: job.ID})
		if err != nil || !dup {
			t.Errorf("late retry = %v, %v", dup, err)
		}
	})
}
