package domain

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// EventDigestMorningBrief is a person's morning brief, sent on the digest
// channel (G15).
const EventDigestMorningBrief = "digest.morning_brief"

// TriggerMorningBrief is the trigger in the input context of a brief job.
const TriggerMorningBrief = "morning_brief"

// Morning brief limits.
const (
	// BriefTopItems is how many tasks the brief puts first.
	BriefTopItems = 3
	// BriefListMax caps the pending approvals and the unblocked tasks the
	// brief lists; their totals are kept.
	BriefListMax = 10
	// BriefTextMax caps the model's text, in characters.
	BriefTextMax = 2000
)

// BriefRecipient is a person the scheduler briefs: someone who has open work
// assigned or a decision waiting, in a workspace that allows the digest
// agent. Users are not stored on their own; they are the IDs on tasks.
type BriefRecipient struct {
	TenantID string
	UserID   string
	// WorkspaceID is one of those workspaces, the first by ID. The job
	// names it so the runtime can check the workspace's settings (D23).
	WorkspaceID string
}

// BriefJobDedupKey is the dedup key of a morning brief: at most one per
// person per UTC calendar day (G15).
func BriefJobDedupKey(tenantID, userID string, day time.Time) string {
	return fmt.Sprintf("%s:%s:%s:brief:%s", AgentDigest, tenantID, userID, day.UTC().Format(time.DateOnly))
}

// BriefInput is the input context of a brief job. The job has no task;
// WorkspaceID is the workspace whose settings govern it (D23).
type BriefInput struct {
	Trigger     string `json:"trigger"`
	UserID      string `json:"user_id"`
	WorkspaceID string `json:"workspace_id"`
	// Date is the brief's UTC date, YYYY-MM-DD.
	Date string `json:"brief_date"`
	// At is when the brief is due, the brief hour on Date. Tasks unblocked
	// in the 24 hours before it count as newly unblocked.
	At time.Time `json:"brief_at"`
}

// Since is the start of the brief's window for newly unblocked tasks: the
// previous day's brief.
func (in BriefInput) Since() time.Time { return in.At.Add(-24 * time.Hour) }

// NewBriefJob returns the digest job that writes r's brief for the day of
// at. The scheduler is the actor, so the job names no requester.
func NewBriefJob(r BriefRecipient, at, now time.Time) (AgentJob, error) {
	if !userIDOK(r.UserID) || r.WorkspaceID == "" {
		return AgentJob{}, invalid("a brief needs a user and a workspace")
	}
	at = at.UTC()
	input, err := json.Marshal(BriefInput{Trigger: TriggerMorningBrief, UserID: r.UserID, WorkspaceID: r.WorkspaceID,
		Date: at.Format(time.DateOnly), At: at})
	if err != nil {
		return AgentJob{}, err
	}
	return NewAgentJob(NewAgentJobInput{
		TenantID:     r.TenantID,
		AgentType:    AgentDigest,
		DedupKey:     BriefJobDedupKey(r.TenantID, r.UserID, at),
		InputContext: input,
	}, now)
}

// ParseBriefInput reads a brief job's input context.
func ParseBriefInput(raw json.RawMessage) (BriefInput, error) {
	var in BriefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return BriefInput{}, invalid("brief input: %v", err)
	}
	if in.Trigger != TriggerMorningBrief || !userIDOK(in.UserID) || in.Date == "" || in.At.IsZero() {
		return BriefInput{}, invalid("brief input is incomplete")
	}
	return in, nil
}

// RankBriefTasks orders a person's open tasks for the brief, most pressing
// first. The order is deterministic and explainable:
//
//  1. overdue tasks (due before at) first;
//  2. then by priority, p1_critical first;
//  3. then by due date, earliest first, tasks with no due date last;
//  4. then work already started (in_progress) before work not started;
//  5. then the oldest task first, and finally by ID.
//
// The priority is the task's own, as for bypassDigest: an agent's
// recommendation does not count until a person approves it.
func RankBriefTasks(tasks []*Task, at time.Time) []*Task {
	out := slices.Clone(tasks)
	slices.SortStableFunc(out, func(a, b *Task) int {
		return cmp.Or(
			-boolCmp(a.overdueAt(at), b.overdueAt(at)),
			cmp.Compare(a.Priority.rank(), b.Priority.rank()),
			compareDue(a.DueAt, b.DueAt),
			-boolCmp(a.Status == StatusInProgress, b.Status == StatusInProgress),
			a.CreatedAt.Compare(b.CreatedAt),
			strings.Compare(a.ID.String(), b.ID.String()),
		)
	})
	return out
}

func (t *Task) overdueAt(at time.Time) bool { return t.DueAt != nil && t.DueAt.Before(at) }

func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// compareDue puts earlier due dates first and missing ones last.
func compareDue(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return a.Compare(*b)
}

// BriefTask is a task as the brief shows it.
type BriefTask struct {
	ID          uuid.UUID  `json:"task_id"`
	Title       string     `json:"title"`
	WorkspaceID string     `json:"workspace_id"`
	Status      Status     `json:"status"`
	Priority    Priority   `json:"priority"`
	DueAt       *time.Time `json:"due_at"`
	Overdue     bool       `json:"overdue"`
}

func briefTask(t *Task, at time.Time) BriefTask {
	return BriefTask{ID: t.ID, Title: t.Title, WorkspaceID: t.WorkspaceID, Status: t.Status, Priority: t.Priority,
		DueAt: utcPtr(t.DueAt), Overdue: t.overdueAt(at)}
}

// BriefApproval is an agent proposal waiting on the person.
type BriefApproval struct {
	JobID     uuid.UUID `json:"job_id"`
	AgentType string    `json:"agent_type"`
	TaskID    uuid.UUID `json:"task_id"`
	TaskTitle string    `json:"task_title"`
	Tools     []string  `json:"proposed_tools"`
	// AskedAt is when the job began to wait.
	AskedAt time.Time `json:"asked_at"`
}

// NewBriefApproval describes a job awaiting approval on task.
func NewBriefApproval(job *AgentJob, task *Task) BriefApproval {
	var proposals []struct {
		Tool string `json:"tool"`
	}
	_ = json.Unmarshal(job.ProposedActions, &proposals) // unreadable proposals list no tools
	tools := []string{}
	for _, p := range proposals {
		tools = append(tools, p.Tool)
	}
	return BriefApproval{JobID: job.ID, AgentType: job.AgentType, TaskID: task.ID, TaskTitle: task.Title,
		Tools: tools, AskedAt: job.UpdatedAt.UTC()}
}

// MorningBrief is one person's brief (feature A4). Go code gathers it; the
// model only writes Text. Its JSON is what the model is shown, what the job
// keeps in execution_result.report, and what GET /api/v1/me/brief returns.
type MorningBrief struct {
	TenantID string    `json:"-"`
	JobID    uuid.UUID `json:"-"`

	Date   string `json:"brief_date"`
	UserID string `json:"user_id"`
	// TopItems are the first BriefTopItems of the person's open tasks, by
	// RankBriefTasks. OpenTasks counts them all.
	TopItems  []BriefTask `json:"top_items"`
	OpenTasks int         `json:"open_tasks"`
	// PendingApprovals are the oldest BriefListMax proposals waiting on the
	// person; PendingApprovalsTotal counts them all.
	PendingApprovals      []BriefApproval `json:"pending_approvals"`
	PendingApprovalsTotal int             `json:"pending_approvals_total"`
	// UnblockedTasks are open tasks unblocked since the previous brief, in
	// rank order; UnblockedTotal counts them all.
	UnblockedTasks []BriefTask `json:"unblocked_tasks"`
	UnblockedTotal int         `json:"unblocked_total"`
	// Text is the brief the model wrote.
	Text string `json:"text,omitempty"`
}

// NewMorningBrief gathers the brief of a job from the person's open tasks
// (todo or in_progress, assigned to them) and the proposals waiting on them,
// both already limited to workspaces that allow the digest agent.
func NewMorningBrief(job *AgentJob, in BriefInput, open []*Task, approvals []BriefApproval) MorningBrief {
	b := MorningBrief{TenantID: job.TenantID, JobID: job.ID, Date: in.Date, UserID: in.UserID,
		TopItems: []BriefTask{}, PendingApprovals: []BriefApproval{}, UnblockedTasks: []BriefTask{}}
	ranked := RankBriefTasks(open, in.At)
	b.OpenTasks = len(ranked)
	since := in.Since()
	for i, t := range ranked {
		if i < BriefTopItems {
			b.TopItems = append(b.TopItems, briefTask(t, in.At))
		}
		if t.UnblockedAt != nil && t.UnblockedAt.After(since) {
			b.UnblockedTotal++
			if len(b.UnblockedTasks) < BriefListMax {
				b.UnblockedTasks = append(b.UnblockedTasks, briefTask(t, in.At))
			}
		}
	}
	waiting := slices.Clone(approvals)
	slices.SortStableFunc(waiting, func(a, b BriefApproval) int {
		return cmp.Or(a.AskedAt.Compare(b.AskedAt), strings.Compare(a.JobID.String(), b.JobID.String()))
	})
	b.PendingApprovalsTotal = len(waiting)
	b.PendingApprovals = append(b.PendingApprovals, waiting[:min(len(waiting), BriefListMax)]...)
	return b
}

// TaskIDs returns every task the brief names, once each: its top items, the
// tasks of its pending approvals, and its unblocked tasks. The model sees
// nothing else, so its text can name no other task.
func (b MorningBrief) TaskIDs() []uuid.UUID {
	var out []uuid.UUID
	add := func(id uuid.UUID) {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	for _, t := range b.TopItems {
		add(t.ID)
	}
	for _, a := range b.PendingApprovals {
		add(a.TaskID)
	}
	for _, t := range b.UnblockedTasks {
		add(t.ID)
	}
	return out
}

// BriefMentions reports whether a stored report, as kept in a digest job's
// execution_result.report, names the task. An unreadable report names
// nothing.
func BriefMentions(report json.RawMessage, taskID uuid.UUID) bool {
	var b MorningBrief
	if len(report) == 0 || json.Unmarshal(report, &b) != nil {
		return false
	}
	return slices.Contains(b.TaskIDs(), taskID)
}

// Empty reports whether the person has nothing to hear about: no open task
// and no decision waiting. An empty brief is not sent.
func (b MorningBrief) Empty() bool {
	return b.OpenTasks == 0 && b.PendingApprovalsTotal == 0
}

// IdempotencyKey is the brief's outbox key: one notification per job, and
// one job per person per day.
func (b MorningBrief) IdempotencyKey() string {
	return fmt.Sprintf("%s:%s:%s:%s", Source, b.TenantID, b.JobID, EventDigestMorningBrief)
}

// Notification is the brief's /notify request: digest channel only, with
// the model's text as payload.digestSummary, the text the digest shows.
// With APP_BASE_URL (AP-10), its view link opens the app's brief page and
// each listed task and approval carries the url of its task's page. Without
// it there is no page to open: the brief carries no actionUrls and its items
// no url, since an API URL needs a bearer token a click cannot carry (D50).
func (b MorningBrief) Notification(links Links) Notification {
	tasks := func(list []BriefTask) []map[string]any {
		out := make([]map[string]any, len(list))
		for i, t := range list {
			m := map[string]any{"taskId": t.ID.String(), "title": t.Title, "workspaceId": t.WorkspaceID,
				"status": string(t.Status), "priority": string(t.Priority), "overdue": t.Overdue}
			if v := links.TaskView(t.ID); v != "" {
				m["url"] = v
			}
			if t.DueAt != nil {
				m["dueAt"] = t.DueAt.UTC().Format(time.RFC3339)
			}
			out[i] = m
		}
		return out
	}
	approvals := make([]map[string]any, len(b.PendingApprovals))
	for i, a := range b.PendingApprovals {
		m := map[string]any{"jobId": a.JobID.String(), "agentType": a.AgentType, "taskId": a.TaskID.String(),
			"taskTitle": a.TaskTitle, "proposedTools": append([]string{}, a.Tools...)}
		// The task's page, where the person decides.
		if v := links.TaskView(a.TaskID); v != "" {
			m["url"] = v
		}
		approvals[i] = m
	}
	payload := map[string]any{
		"jobId":                 b.JobID.String(),
		"agentType":             AgentDigest,
		"briefDate":             b.Date,
		"digestSummary":         b.Text,
		"topItems":              tasks(b.TopItems),
		"openTasks":             b.OpenTasks,
		"pendingApprovals":      approvals,
		"pendingApprovalsTotal": b.PendingApprovalsTotal,
		"unblockedTasks":        tasks(b.UnblockedTasks),
		"unblockedTotal":        b.UnblockedTotal,
	}
	if v := links.BriefView(); v != "" {
		// Not about one task: a plain link only.
		payload["actionUrls"] = map[string]string{ActionView: v}
	}
	return Notification{
		IdempotencyKey: b.IdempotencyKey(),
		TenantID:       b.TenantID,
		UserID:         b.UserID,
		Source:         Source,
		EventType:      EventDigestMorningBrief,
		Title:          "Your morning brief for " + b.Date,
		Body:           b.Text,
		PriorityHint:   PriorityMedium.Hint(),
		Channels:       []string{ChannelDigest},
		Payload:        payload,
	}
}
