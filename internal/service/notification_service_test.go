package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

type fakeFeed struct {
	body json.RawMessage
	err  error
	user string
}

func (f *fakeFeed) InAppFeed(ctx context.Context, tenantID, userID string, _ int, _ string) (json.RawMessage, error) {
	f.user = tenantID + "/" + userID
	if f.err != nil {
		return nil, f.err
	}
	return f.body, nil
}

func newNotifications(feed FeedSource) *NotificationService {
	b := repotest.Memory()
	return NewNotificationService(b.Tx, feed, b.Tasks, b.Acks, b.Outbox, 3, nil, nil, nil)
}

func TestFeedUpstreamFailures(t *testing.T) {
	for name, err := range map[string]error{
		"breaker open": agentpulse.ErrCircuitOpen,
		"timeout":      context.DeadlineExceeded,
		"server error": &agentpulse.StatusError{Code: 502},
		"rate limited": &agentpulse.StatusError{Code: 429},
	} {
		_, got := newNotifications(&fakeFeed{err: err}).Feed(context.Background(), "tenant_acme", "usr_bob", 0, "")
		if !errors.Is(got, ErrFeedUnavailable) {
			t.Errorf("%s: %v", name, got)
		}
	}
	// A refusal is not "try again": the caller sees the original error.
	_, got := newNotifications(&fakeFeed{err: &agentpulse.StatusError{Code: 400}}).Feed(context.Background(), "tenant_acme", "usr_bob", 0, "")
	if got == nil || errors.Is(got, ErrFeedUnavailable) {
		t.Errorf("400: %v", got)
	}
	_, got = newNotifications(&fakeFeed{body: json.RawMessage(`not json`)}).Feed(context.Background(), "tenant_acme", "usr_bob", 0, "")
	if !errors.Is(got, ErrFeedUnavailable) {
		t.Errorf("unreadable feed: %v", got)
	}
	// A cancelled caller is not an AgentPulse outage.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, got := newNotifications(&fakeFeed{err: ctx.Err()}).Feed(ctx, "tenant_acme", "usr_bob", 0, ""); errors.Is(got, ErrFeedUnavailable) {
		t.Errorf("cancelled: %v", got)
	}
}

func TestFeedInputsAndSkippedItems(t *testing.T) {
	feed := &fakeFeed{body: json.RawMessage(`{"notifications":[
		{"id":"ok","eventType":"x","title":"t","body":"b","createdAt":"2026-11-20T10:00:00Z","payload":{"taskId":"nope"}},
		{"id":"","createdAt":"2026-11-20T10:00:00Z"},
		{"id":"bad date","createdAt":"yesterday"},
		{"id":"nodate"}],"nextCursor":"7"}`)}
	s := newNotifications(feed)
	page, err := s.Feed(context.Background(), "tenant_acme", "usr_bob", 5, "3")
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "ok" || page.Items[0].TaskID != nil || page.Next != "7" {
		t.Fatalf("page = %+v, %v", page, err)
	}
	if feed.user != "tenant_acme/usr_bob" {
		t.Errorf("asked AgentPulse for %q", feed.user)
	}
	for name, call := range map[string]func() error{
		"tenant": func() error { _, e := s.Feed(context.Background(), "default", "usr_bob", 0, ""); return e },
		"user":   func() error { _, e := s.Feed(context.Background(), "tenant_acme", "", 0, ""); return e },
		"limit":  func() error { _, e := s.Feed(context.Background(), "tenant_acme", "usr_bob", 101, ""); return e },
		"cursor": func() error {
			_, e := s.Feed(context.Background(), "tenant_acme", "usr_bob", 0, string(make([]byte, 600)))
			return e
		},
		"ack id": func() error {
			_, e := s.Ack(context.Background(), AckInput{TenantID: "tenant_acme", UserID: "u", NotificationID: "a b", Signal: "acted"})
			return e
		},
		"ack sig": func() error {
			_, e := s.Ack(context.Background(), AckInput{TenantID: "tenant_acme", UserID: "u", NotificationID: "n", Signal: "x"})
			return e
		},
		"ack ten": func() error {
			_, e := s.Ack(context.Background(), AckInput{TenantID: "", UserID: "u", NotificationID: "n", Signal: "acted"})
			return e
		},
		"ack usr": func() error {
			_, e := s.Ack(context.Background(), AckInput{TenantID: "tenant_acme", UserID: "", NotificationID: "n", Signal: "acted"})
			return e
		},
	} {
		if err := call(); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	_ = time.Second
}
