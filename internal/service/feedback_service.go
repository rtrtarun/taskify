package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// Feedback signals, as AgentPulse names them.
const (
	SignalActed     = "acted"
	SignalDismissed = "dismissed"
	SignalSnoozed   = "snoozed"
	SignalMuted     = "muted"
)

// MaxSnooze is the longest snooze accepted.
const MaxSnooze = 7 * 24 * time.Hour

// FeedbackSender forwards a signal to AgentPulse.
type FeedbackSender interface {
	RecordFeedback(ctx context.Context, fb agentpulse.Feedback) error
}

// FeedbackService forwards users' reactions to notifications. It never
// changes a task.
type FeedbackService struct {
	tasks  repository.TaskRepository
	sender FeedbackSender
}

// NewFeedbackService returns a FeedbackService.
func NewFeedbackService(tasks repository.TaskRepository, sender FeedbackSender) *FeedbackService {
	return &FeedbackService{tasks: tasks, sender: sender}
}

// FeedbackInput is one reaction from a user.
type FeedbackInput struct {
	TenantID       string
	UserID         string
	TaskID         uuid.UUID
	NotificationID string
	Signal         string
	// Duration applies to snoozed only, such as "2h".
	Duration string
}

// Record validates the reaction, checks the task exists in the caller's
// tenant, and forwards the signal to AgentPulse.
func (s *FeedbackService) Record(ctx context.Context, in FeedbackInput) error {
	if _, err := ParseSignal(in.Signal, in.Duration); err != nil {
		return err
	}
	if len(in.NotificationID) > 128 {
		return fmt.Errorf("%w: notification_id must be at most 128 characters", domain.ErrValidation)
	}
	if _, err := s.tasks.Get(ctx, in.TenantID, in.TaskID); err != nil {
		return err
	}
	return s.forward(ctx, in)
}

func (s *FeedbackService) forward(ctx context.Context, in FeedbackInput) error {
	ctx, cancel := context.WithTimeout(ctx, agentPulseTimeout)
	defer cancel()
	fb := agentpulse.Feedback{
		NotificationID: in.NotificationID,
		TenantID:       in.TenantID,
		UserID:         in.UserID,
		Signal:         in.Signal,
		TaskID:         in.TaskID.String(),
	}
	if in.Signal == SignalSnoozed {
		fb.SnoozeDuration = in.Duration
	}
	return s.sender.RecordFeedback(ctx, fb)
}

// ParseSignal validates a signal and, for snoozed, its duration, which is
// returned. Snoozed requires a duration; other signals reject one.
func ParseSignal(signal, duration string) (time.Duration, error) {
	switch signal {
	case SignalActed, SignalDismissed, SignalMuted:
		if strings.TrimSpace(duration) != "" {
			return 0, fmt.Errorf("%w: duration applies only to snoozed", domain.ErrValidation)
		}
		return 0, nil
	case SignalSnoozed:
		d, err := time.ParseDuration(strings.TrimSpace(duration))
		if err != nil || d <= 0 || d > MaxSnooze {
			return 0, fmt.Errorf("%w: snoozed needs a duration such as 2h, at most 168h", domain.ErrValidation)
		}
		return d, nil
	}
	return 0, fmt.Errorf("%w: signal must be acted, dismissed, snoozed, or muted", domain.ErrValidation)
}

// Webhook actions from AgentPulse. See docs/implementation/README.md, G7.
const (
	ActionComplete = "complete"
	ActionSnooze   = "snooze"
)

// Callback is an authenticated action from AgentPulse, such as a "Mark
// done" click in Slack.
type Callback struct {
	// Key identifies the delivery; a repeated key is ignored. The webhook
	// handler uses the request signature.
	Key            string
	Action         string
	TenantID       string
	UserID         string
	TaskID         uuid.UUID
	NotificationID string
	Duration       string // for snooze
}

// WebhookService applies callbacks from AgentPulse.
type WebhookService struct {
	tx         repository.TxManager
	deliveries repository.WebhookDeliveryRepository
	tasks      *TaskService
	feedback   *FeedbackService
	logger     *slog.Logger
	now        func() time.Time
}

// NewWebhookService returns a WebhookService.
func NewWebhookService(tx repository.TxManager, d repository.WebhookDeliveryRepository, tasks *TaskService, fb *FeedbackService, logger *slog.Logger, now func() time.Time) *WebhookService {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &WebhookService{tx: tx, deliveries: d, tasks: tasks, feedback: fb, logger: logger, now: now}
}

// Handle applies a callback once. It records the delivery and changes the
// task in one transaction, so a failed change can be retried and a repeated
// delivery changes nothing; duplicate reports the latter. After the change
// commits, the matching feedback signal is forwarded to AgentPulse. A
// forwarding failure is logged, not returned: the user's action has already
// taken effect.
func (s *WebhookService) Handle(ctx context.Context, cb Callback) (duplicate bool, err error) {
	if strings.TrimSpace(cb.TenantID) == "" || strings.TrimSpace(cb.UserID) == "" || cb.TaskID == uuid.Nil {
		return false, fmt.Errorf("%w: tenantId, userId, and taskId are required", domain.ErrValidation)
	}
	var signal string
	var snooze time.Duration
	switch cb.Action {
	case ActionComplete:
		signal = SignalActed
	case ActionSnooze:
		signal = SignalSnoozed
		if snooze, err = ParseSignal(SignalSnoozed, cb.Duration); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("%w: unknown action %q", domain.ErrValidation, cb.Action)
	}

	actor := domain.User(cb.UserID)
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		fresh, err := s.deliveries.Record(ctx, cb.Key, s.now().UTC())
		if err != nil {
			return err
		}
		if !fresh {
			duplicate = true
			return nil
		}
		if cb.Action == ActionComplete {
			_, err = s.tasks.CompleteTask(ctx, actor, cb.TenantID, cb.TaskID, nil)
		} else {
			_, err = s.tasks.SnoozeTask(ctx, actor, cb.TenantID, cb.TaskID, snooze)
		}
		return err
	})
	if err != nil || duplicate {
		return duplicate, err
	}

	fbErr := s.feedback.forward(ctx, FeedbackInput{
		TenantID: cb.TenantID, UserID: cb.UserID, TaskID: cb.TaskID,
		NotificationID: cb.NotificationID, Signal: signal, Duration: cb.Duration,
	})
	if fbErr != nil {
		s.logger.Warn("webhook: action applied but feedback not forwarded",
			"tenant_id", cb.TenantID, "task_id", cb.TaskID, "signal", signal, "error", fbErr)
	}
	return false, nil
}
