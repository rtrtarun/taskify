package domain_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/contract"
	"github.com/rtrtarun/taskify/internal/domain"
)

// outboxEventTypes is every event_type an outbox row can have: the 16
// notifications and the three other calls the relay makes.
var outboxEventTypes = []string{
	domain.EventTaskCreated, domain.EventTaskAssigned, domain.EventTaskUpdated, domain.EventTaskPriorityEscalated,
	domain.EventTaskBlocked, domain.EventTaskDueSoon, domain.EventTaskOverdue, domain.EventTaskCompleted,
	domain.EventTaskCommentAdded, domain.EventTaskUnblocked, domain.EventTaskDependencyResolved,
	domain.EventTaskDependencyAdded, domain.EventTaskDependencyReopened, domain.EventAgentApprovalRequested,
	domain.EventTaskEscalated, domain.EventDigestMorningBrief, domain.EventContactUpdate, domain.EventLearningSuggestion, domain.EventFeedbackForward,
}

// TestContractDefinesEveryOutboxEvent: contract/events has one schema per
// outbox event type, and none for a type the server never writes (M12-02).
func TestContractDefinesEveryOutboxEvent(t *testing.T) {
	want := slices.Clone(outboxEventTypes)
	slices.Sort(want)
	if got := contract.EventTypes(); !slices.Equal(got, want) {
		t.Errorf("contract/events = %v, want %v", got, want)
	}
}

// TestContactAndLearningPayloadsMatchTheContract covers the two outbox rows
// that are not notifications. TestEveryNotificationPassesAgentPulseActionChecks
// covers the other 16 types.
func TestContactAndLearningPayloadsMatchTheContract(t *testing.T) {
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	email, slack := "bob@example.com", "U123ABC"
	for _, c := range []domain.Contact{
		{TenantID: "tenant_acme", UserID: "usr_bob", Email: &email, SlackUserID: &slack},
		{TenantID: "tenant_acme", UserID: "usr_bob"}, // cleared
	} {
		e, err := domain.NewContactEntry(c, 10, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := contract.ValidateEvent(e.EventType, e.Payload); err != nil {
			t.Errorf("%s: %v", e.Payload, err)
		}
	}
	label := "bug"
	for _, l := range []*string{&label, nil} {
		e, err := domain.NewLearningSuggestionEntry(domain.LearningSuggestion{TenantID: "tenant_acme", JobID: uuid.New(),
			Pattern: domain.EventPattern{EventType: domain.EventTaskUpdated, Label: l}, CurrentAdjustment: 0,
			ProposedAdjustment: -5, SignalCount: 7, Rationale: "Dismissed by most."}, 10, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := contract.ValidateEvent(e.EventType, e.Payload); err != nil {
			t.Errorf("%s: %v", e.Payload, err)
		}
	}
}

// TestFeedbackForwardPayloadMatchesTheContract: the row M14-06 writes for an
// ack validates against its schema, with and without a snooze.
func TestFeedbackForwardPayloadMatchesTheContract(t *testing.T) {
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	for _, a := range []domain.NotificationAck{
		{TenantID: "tenant_acme", UserID: "usr_bob", NotificationID: "ntf-1", Signal: domain.AckDismissed, AckedAt: now},
		{TenantID: "tenant_acme", UserID: "usr_bob", NotificationID: "ntf-1", Signal: domain.AckSnoozed, SnoozeDuration: "2h", AckedAt: now},
	} {
		e, err := domain.NewFeedbackEntry(a, 10)
		if err != nil {
			t.Fatal(err)
		}
		if err := contract.ValidateEvent(e.EventType, e.Payload); err != nil {
			t.Errorf("%s: %v (%s)", a.Signal, err, e.Payload)
		}
	}
	if _, err := domain.NewFeedbackEntry(domain.NotificationAck{TenantID: "tenant_acme", UserID: "u", NotificationID: "a b", Signal: "acted"}, 10); err == nil {
		t.Error("an ID with a space was accepted")
	}
}
