package service

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/observability"
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
	tasks   repository.TaskRepository
	sender  FeedbackSender
	metrics *observability.Metrics
}

// NewFeedbackService returns a FeedbackService. metrics may be nil.
func NewFeedbackService(tasks repository.TaskRepository, sender FeedbackSender, metrics *observability.Metrics) *FeedbackService {
	return &FeedbackService{tasks: tasks, sender: sender, metrics: metrics}
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
	if err := s.sender.RecordFeedback(ctx, fb); err != nil {
		return err
	}
	s.metrics.FeedbackForwarded(in.Signal)
	return nil
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
	// ActionUnblock moves a blocked task to in_progress, from a blocker
	// escalation.
	ActionUnblock = domain.ActionUnblock
	// ActionApprove and ActionReject decide an agent job's proposals.
	ActionApprove = "approve"
	ActionReject  = "reject"
)

// Callback is an authenticated action from AgentPulse, such as a "Mark
// done" click in Slack.
type Callback struct {
	// Key identifies the delivery when CallbackID is empty: a repeated key
	// is ignored. The webhook handler uses the request signature, so only a
	// byte-identical replay repeats it (D10).
	//
	// CallbackID, when AgentPulse sends one, identifies the click: it is the
	// same on every retry of one click, while the timestamp and signature
	// change. A callback is a duplicate when its tenant has seen the
	// CallbackID before (AP-06).
	Key        string
	CallbackID string
	Action     string
	TenantID   string
	UserID     string
	TaskID     uuid.UUID
	// JobID names the agent job, for approve and reject.
	JobID          uuid.UUID
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
	agentJobs  *AgentJobService
}

// WithAgentJobs enables the approve and reject actions.
func (s *WebhookService) WithAgentJobs(j *AgentJobService) *WebhookService {
	s.agentJobs = j
	return s
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

// callbackIDRe is the form of AgentPulse's callbackId.
var callbackIDRe = regexp.MustCompile(`^cb-[0-9a-f]{32}$`)

// CallbackRetention is how long a callbackId is remembered: the 7-day life
// of an AgentPulse action link.
const CallbackRetention = 7 * 24 * time.Hour

// record stores the callback's identity, its CallbackID or else its Key,
// and reports whether it was new.
func (s *WebhookService) record(ctx context.Context, cb Callback) (bool, error) {
	if cb.CallbackID != "" {
		return s.deliveries.RecordCallback(ctx, cb.TenantID, cb.CallbackID, s.now().UTC())
	}
	return s.deliveries.Record(ctx, cb.Key, s.now().UTC())
}

// Handle applies a callback once. It records the delivery and changes the
// task in one transaction, so a failed change can be retried and a repeated
// delivery changes nothing; duplicate reports the latter. A delivery is the
// tenant's callbackId when AgentPulse sends one, so a retry signed again
// with a new timestamp is still a duplicate; without one it is the request
// signature (D10). After the change commits, the matching feedback signal is
// forwarded to AgentPulse. A forwarding failure is logged, not returned: the
// user's action has already taken effect.
//
// Two deliveries of one callback at once, on any replicas, insert the same
// record: the second insert waits for the first transaction and then finds
// the record, so exactly one applies the change and forwards feedback.
func (s *WebhookService) Handle(ctx context.Context, cb Callback) (duplicate bool, err error) {
	// The tenant comes from AgentPulse, not from a token: it must still be
	// one Taskify could have (AP-04).
	if err := domain.ValidateTenantID(cb.TenantID); err != nil {
		return false, err
	}
	// So must the user, who appears in the keys of the notifications the
	// action sends (AP-11). A missing one is reported with the other
	// required fields below.
	if strings.TrimSpace(cb.UserID) != "" {
		if err := domain.ValidateUserID("userId", cb.UserID); err != nil {
			return false, err
		}
	}
	if cb.CallbackID != "" && !callbackIDRe.MatchString(cb.CallbackID) {
		return false, fmt.Errorf("%w: callbackId must be cb- and 32 lower-case hex digits", domain.ErrValidation)
	}
	if cb.Action == ActionApprove || cb.Action == ActionReject {
		return s.decide(ctx, cb)
	}
	if strings.TrimSpace(cb.UserID) == "" || cb.TaskID == uuid.Nil {
		return false, fmt.Errorf("%w: tenantId, userId, and taskId are required", domain.ErrValidation)
	}
	var signal string
	var snooze time.Duration
	switch cb.Action {
	case ActionComplete, ActionUnblock:
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
		fresh, err := s.record(ctx, cb)
		if err != nil {
			return err
		}
		if !fresh {
			duplicate = true
			return nil
		}
		switch cb.Action {
		case ActionComplete:
			_, err = s.tasks.CompleteTask(ctx, actor, cb.TenantID, cb.TaskID, nil)
		case ActionUnblock:
			_, err = s.tasks.UnblockTask(ctx, actor, cb.TenantID, cb.TaskID)
		default:
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

// decide approves or rejects an agent job's proposals for the user named in
// the callback, through the same AgentJobService as the API and with the
// same permission check (G10). AgentPulse does not say whether the user is
// an admin, so a callback decides as a member.
//
// The decision is idempotent by itself: approving an approved job changes
// nothing. A callbackId seen before is a duplicate at once. Otherwise the
// delivery is recorded after the decision, which commits on its own (a stale
// approval fails the job and must stay failed), and feedback is forwarded
// only when the decision changed the job, so a repeated callback sends no
// second signal, even when two arrive at once: the job's row lock lets one
// of them change it. duplicate reports a callback that changed nothing.
func (s *WebhookService) decide(ctx context.Context, cb Callback) (duplicate bool, err error) {
	if strings.TrimSpace(cb.UserID) == "" || cb.JobID == uuid.Nil {
		return false, fmt.Errorf("%w: tenantId, userId, and jobId are required", domain.ErrValidation)
	}
	if s.agentJobs == nil {
		return false, fmt.Errorf("%w: agent approvals are not available", domain.ErrValidation)
	}
	if cb.CallbackID != "" {
		seen, err := s.deliveries.CallbackSeen(ctx, cb.TenantID, cb.CallbackID)
		if err != nil {
			return false, err
		}
		if seen {
			return true, nil
		}
	}
	who := Decider{UserID: cb.UserID}
	var (
		job     *domain.AgentJob
		changed bool
	)
	signal := SignalActed
	if cb.Action == ActionApprove {
		job, changed, err = s.agentJobs.Apply(ctx, cb.TenantID, cb.JobID, who)
	} else {
		signal = SignalDismissed
		job, changed, err = s.agentJobs.Dismiss(ctx, cb.TenantID, cb.JobID, who)
	}
	if err != nil {
		return false, err
	}
	if err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.record(ctx, cb)
		return err
	}); err != nil {
		s.logger.Warn("webhook: decision applied but delivery not recorded", "job_id", cb.JobID, "error", err)
	}
	if !changed {
		return true, nil
	}
	if job.TaskID != nil {
		fbErr := s.feedback.forward(ctx, FeedbackInput{
			TenantID: cb.TenantID, UserID: cb.UserID, TaskID: *job.TaskID,
			NotificationID: cb.NotificationID, Signal: signal,
		})
		if fbErr != nil {
			s.logger.Warn("webhook: decision applied but feedback not forwarded",
				"tenant_id", cb.TenantID, "job_id", cb.JobID, "signal", signal, "error", fbErr)
		}
	}
	return false, nil
}
