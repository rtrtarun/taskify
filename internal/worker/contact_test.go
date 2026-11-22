package worker

import (
	"context"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
	"github.com/rtrtarun/taskify/internal/service"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

const contactPath = "/v1/tenants/" + "tenant_acme" + "/users/usr_bob/contact"

func setEmail(v string) domain.ContactPatch {
	return domain.ContactPatch{Email: domain.ContactField{Set: true, Value: &v}}
}

func contactRow(t *testing.T, b repotest.Backend) domain.OutboxEntry {
	t.Helper()
	rows, err := b.Outbox.ListForAggregate(ctx, domain.ContactAggregateID(tenant, "usr_bob"))
	if err != nil || len(rows) != 1 {
		t.Fatalf("contact rows = %d, %v", len(rows), err)
	}
	return rows[0]
}

// TestContactForwardSurvivesAFailedAttempt: one update yields one accepted
// AgentPulse call, even when the first attempt fails (AP-05).
func TestContactForwardSurvivesAFailedAttempt(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		mock, client := newMock(t)
		contacts := service.NewContactService(b.Tx, b.Contacts, b.Outbox, 10, nil, nil)
		if _, err := contacts.Update(ctx, tenant, "usr_bob", setEmail("bob@example.com")); err != nil {
			t.Fatal(err)
		}
		mock.FailNext("PUT", contactPath, http.StatusServiceUnavailable, 1)
		r := NewRelay(b.Outbox, client, RelayConfig{Backoff: func(int) time.Duration { return 0 }, Logger: quiet})
		for i := 0; i < 2; i++ {
			if n, err := r.RunOnce(ctx); err != nil || n != 1 {
				t.Fatalf("run %d: %d rows, %v", i, n, err)
			}
		}
		if n, _ := r.RunOnce(ctx); n != 0 {
			t.Errorf("a third run sent %d rows", n)
		}
		if got := len(mock.RequestsTo("PUT", contactPath)); got != 2 {
			t.Errorf("PUT requests = %d, want 2 (one failed)", got)
		}
		if got := mock.ContactPuts(); got != 1 {
			t.Errorf("accepted PUTs = %d, want 1", got)
		}
		if c, ok := mock.ContactOf(tenant, "usr_bob"); !ok || c.Email != "bob@example.com" {
			t.Errorf("AgentPulse holds %+v", c)
		}
		if row := contactRow(t, b); row.Status != domain.OutboxDispatched || row.Attempts != 1 {
			t.Errorf("row = %s after %d attempts", row.Status, row.Attempts)
		}
		// The body is the full replace, every field present.
		if body := string(mock.RequestsTo("PUT", contactPath)[1].Body); body != `{"email":"bob@example.com","slackUserId":null,"phone":null}` {
			t.Errorf("body = %s", body)
		}
		// Setting the same contact again sends nothing.
		if _, err := contacts.Update(ctx, tenant, "usr_bob", setEmail("bob@example.com")); err != nil {
			t.Fatal(err)
		}
		if n, _ := r.RunOnce(ctx); n != 0 {
			t.Errorf("an unchanged contact sent %d rows", n)
		}
	})
}

// TestContactUpdatesBeforeASendCoalesce: two updates before the relay runs
// make one row and one call, carrying the newer contact.
func TestContactUpdatesBeforeASendCoalesce(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		mock, client := newMock(t)
		contacts := service.NewContactService(b.Tx, b.Contacts, b.Outbox, 10, nil, nil)
		for _, e := range []string{"old@example.com", "new@example.com"} {
			if _, err := contacts.Update(ctx, tenant, "usr_bob", setEmail(e)); err != nil {
				t.Fatal(err)
			}
		}
		r := NewRelay(b.Outbox, client, RelayConfig{Logger: quiet})
		if n, err := r.RunOnce(ctx); n != 1 || err != nil {
			t.Fatalf("run: %d, %v", n, err)
		}
		if mock.ContactPuts() != 1 {
			t.Errorf("PUTs = %d, want 1", mock.ContactPuts())
		}
		if c, _ := mock.ContactOf(tenant, "usr_bob"); c.Email != "new@example.com" {
			t.Errorf("AgentPulse holds %+v", c)
		}
	})
}

// TestContactUpdateDuringASend: an update that commits while the previous
// contact is in flight is sent after it, so AgentPulse ends with the newer
// one, whether the in-flight send succeeds or is refused.
func TestContactUpdateDuringASend(t *testing.T) {
	for _, refused := range []bool{false, true} {
		name := "send succeeds"
		if refused {
			name = "send refused"
		}
		t.Run(name, func(t *testing.T) {
			each(t, func(t *testing.T, b repotest.Backend) {
				mock, client := newMock(t)
				contacts := service.NewContactService(b.Tx, b.Contacts, b.Outbox, 10, nil, nil)
				if _, err := contacts.Update(ctx, tenant, "usr_bob", setEmail("old@example.com")); err != nil {
					t.Fatal(err)
				}
				mock.SetDelay("PUT", contactPath, 300*time.Millisecond)
				if refused {
					mock.FailNext("PUT", contactPath, http.StatusBadRequest, 1)
				}
				r := NewRelay(b.Outbox, client, RelayConfig{Logger: quiet})
				done := make(chan struct{})
				go func() {
					defer close(done)
					if _, err := r.RunOnce(ctx); err != nil {
						t.Error(err)
					}
				}()
				waitFor(t, "the first PUT to reach AgentPulse", func() bool { return len(mock.RequestsTo("PUT", contactPath)) == 1 })
				if _, err := contacts.Update(ctx, tenant, "usr_bob", setEmail("new@example.com")); err != nil {
					t.Fatal(err)
				}
				<-done
				if row := contactRow(t, b); row.Status != domain.OutboxPending || row.Attempts != 0 {
					t.Fatalf("after the in-flight send: %s, %d attempts", row.Status, row.Attempts)
				}
				mock.SetDelay("PUT", contactPath, 0)
				if n, err := r.RunOnce(ctx); n != 1 || err != nil {
					t.Fatalf("second run: %d, %v", n, err)
				}
				if c, _ := mock.ContactOf(tenant, "usr_bob"); c.Email != "new@example.com" {
					t.Errorf("AgentPulse holds %+v, want the newer contact", c)
				}
				reqs := mock.RequestsTo("PUT", contactPath)
				if len(reqs) != 2 || !strings.Contains(string(reqs[0].Body), "old@") || !strings.Contains(string(reqs[1].Body), "new@") {
					t.Errorf("PUTs in order: %d", len(reqs))
				}
				if row := contactRow(t, b); row.Status != domain.OutboxDispatched {
					t.Errorf("row = %s", row.Status)
				}
			})
		})
	}
}

// inFlight wraps the mock and records the most contact PUTs it ever had in
// flight for one person, holding each for a random moment.
type inFlight struct {
	next    http.Handler
	now     atomic.Int32
	highest atomic.Int32
}

func (h *inFlight) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/contact") {
		n := h.now.Add(1)
		defer h.now.Add(-1)
		for {
			top := h.highest.Load()
			if n <= top || h.highest.CompareAndSwap(top, n) {
				break
			}
		}
		time.Sleep(time.Duration(rand.IntN(15)) * time.Millisecond)
	}
	h.next.ServeHTTP(w, r)
}

// TestContactOrderAcrossReplicas: two relays, as two replicas, send a burst
// of one person's updates. One person's contact is never in flight twice,
// and AgentPulse ends with the latest stored contact.
func TestContactOrderAcrossReplicas(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		mock := mockagentpulse.New()
		wrapped := &inFlight{next: mock}
		srv := httptest.NewServer(wrapped)
		t.Cleanup(srv.Close)
		client := agentpulse.New(srv.URL, "k", agentpulse.WithBreaker(nil))
		contacts := service.NewContactService(b.Tx, b.Contacts, b.Outbox, 10, nil, nil)

		runCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			r := NewRelay(b.Outbox, client, RelayConfig{Workers: 4, PollInterval: 2 * time.Millisecond, Logger: quiet})
			wg.Add(1)
			go func() { defer wg.Done(); _ = r.Run(runCtx) }()
		}
		var last string
		for i := 0; i < 30; i++ {
			last = "v" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@example.com"
			if _, err := contacts.Update(ctx, tenant, "usr_bob", setEmail(last)); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Duration(rand.IntN(6)) * time.Millisecond)
		}
		waitFor(t, "the latest contact at AgentPulse", func() bool {
			c, _ := mock.ContactOf(tenant, "usr_bob")
			rows, _ := b.Outbox.ListForAggregate(ctx, domain.ContactAggregateID(tenant, "usr_bob"))
			return c.Email == last && len(rows) == 1 && rows[0].Status == domain.OutboxDispatched
		})
		cancel()
		wg.Wait()
		if got := wrapped.highest.Load(); got != 1 {
			t.Errorf("up to %d PUTs for one person were in flight at once", got)
		}
		stored, _ := contacts.Get(ctx, tenant, "usr_bob")
		if c, _ := mock.ContactOf(tenant, "usr_bob"); stored.Email == nil || c.Email != *stored.Email {
			t.Errorf("AgentPulse holds %+v, Taskify %v", c, stored.Email)
		}
		t.Logf("%d updates, %d PUTs", 30, mock.ContactPuts())
	})
}

// TestContactRefusedByAgentPulseIsDeadLettered is AP-12: a contact PUT that
// AgentPulse refuses with 400 is permanent. The row goes to dead letter on
// its first attempt with the reason, naming the field, and is not retried.
// The row is written directly here: Taskify's own validation, which matches
// AgentPulse's, would refuse the value before it reached the outbox.
func TestContactRefusedByAgentPulseIsDeadLettered(t *testing.T) {
	each(t, func(t *testing.T, b repotest.Backend) {
		mock, client := newMock(t)
		email, phone := "bob@example.com", "+0123"
		entry, err := domain.NewContactEntry(domain.Contact{TenantID: tenant, UserID: "usr_bob", Email: &email, Phone: &phone}, 10, t0)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Outbox.Coalesce(ctx, entry); err != nil {
			t.Fatal(err)
		}
		r := NewRelay(b.Outbox, client, RelayConfig{Backoff: func(int) time.Duration { return 0 }, Logger: quiet})
		if n, err := r.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("run: %d rows, %v", n, err)
		}
		row := contactRow(t, b)
		if row.Status != domain.OutboxDeadLetter || row.Attempts != 1 || !strings.Contains(row.LastError, "400") ||
			!strings.Contains(row.LastError, "phone must be") {
			t.Errorf("row = %s after %d attempts: %q", row.Status, row.Attempts, row.LastError)
		}
		if n, _ := r.RunOnce(ctx); n != 0 {
			t.Errorf("a refused contact was sent again: %d rows", n)
		}
		if got := len(mock.RequestsTo("PUT", contactPath)); got != 1 {
			t.Errorf("PUT requests = %d, want 1", got)
		}
		if _, ok := mock.ContactOf(tenant, "usr_bob"); ok {
			t.Error("AgentPulse stored a refused contact")
		}
	})
}
