package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
	"github.com/rtrtarun/taskify/internal/service"
)

// The list endpoints the clients need (M12-04), described in
// contract/openapi.yaml first.

// CodeNotDeadLetter answers a replay of an outbox row that is not in dead
// letter, with status 409.
const CodeNotDeadLetter = "not_dead_letter"

// pageParams reads limit and cursor, shared by every list.
func pageParams(r *http.Request) (int, *repository.Cursor, error) {
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > service.MaxListLimit {
			return 0, nil, badRequest("limit must be an integer from 1 to 200")
		}
		limit = n
	}
	if v := q.Get("cursor"); v != "" {
		c, err := decodeCursor(v)
		if err != nil {
			return 0, nil, err
		}
		return limit, c, nil
	}
	return limit, nil, nil
}

// listCursor converts a decoded cursor for the lists ordered by creation.
func listCursor(c *repository.Cursor) *repository.ListCursor {
	if c == nil {
		return nil
	}
	return &repository.ListCursor{CreatedAt: c.UpdatedAt, ID: c.ID}
}

func encodeListCursor(c *repository.ListCursor) *string {
	if c == nil {
		return nil
	}
	return encodeCursor(&repository.Cursor{UpdatedAt: c.CreatedAt, ID: c.ID})
}

type meJSON struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	Role     string `json:"role"`
}

// getMe serves GET /me: who the server takes the caller to be.
func (h *handlers) getMe(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	writeJSON(w, http.StatusOK, meJSON{TenantID: id.TenantID, UserID: id.UserID, Role: id.Role})
}

type agentJobListJSON struct {
	AgentJobs  []agentJobJSON `json:"agent_jobs"`
	NextCursor *string        `json:"next_cursor"`
}

// listAgentJobs serves GET /agent-jobs, filtered by status, task, and agent
// type: the approvals inbox reads status=awaiting_approval.
func (h *handlers) listAgentJobs(w http.ResponseWriter, r *http.Request) {
	limit, after, err := pageParams(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	f := repository.AgentJobFilter{TenantID: identity(r).TenantID, Status: domain.JobStatus(q.Get("status")),
		AgentType: q.Get("agent_type"), Limit: limit, After: listCursor(after)}
	if v := q.Get("task_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			h.fail(w, r, badRequest("task_id must be a task ID"))
			return
		}
		f.TaskID = &id
	}
	page, err := h.agentJobs.List(r.Context(), f, decider(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := agentJobListJSON{AgentJobs: make([]agentJobJSON, 0, len(page.Jobs)), NextCursor: encodeListCursor(page.Next)}
	for _, j := range page.Jobs {
		out.AgentJobs = append(out.AgentJobs, toAgentJobJSON(j))
	}
	writeJSON(w, http.StatusOK, out)
}

type subtaskJSON struct {
	Task      taskJSON    `json:"task"`
	DependsOn []uuid.UUID `json:"depends_on"`
}

// listSubtasks serves GET /tasks/{id}/subtasks.
func (h *handlers) listSubtasks(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	subtasks, err := h.tasks.ListSubtasks(r.Context(), identity(r).TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]subtaskJSON, 0, len(subtasks))
	for _, s := range subtasks {
		out = append(out, subtaskJSON{Task: toTaskJSON(s.Task), DependsOn: s.DependsOn})
	}
	writeJSON(w, http.StatusOK, map[string]any{"subtasks": out})
}

type agentRunJSON struct {
	Turn             int             `json:"turn"`
	Model            string          `json:"model"`
	ToolName         *string         `json:"tool_name"`
	ToolArgs         json.RawMessage `json:"tool_args"`
	ToolResult       json.RawMessage `json:"tool_result"`
	Outcome          string          `json:"outcome"`
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	LatencyMS        int             `json:"latency_ms"`
	CreatedAt        string          `json:"created_at"`
}

type agentJobRunsJSON struct {
	Job  agentJobJSON   `json:"job"`
	Runs []agentRunJSON `json:"runs"`
}

// listAgentRuns serves GET /tasks/{id}/agent-runs: the task's agent jobs
// with their run history.
func (h *handlers) listAgentRuns(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	jobs, err := h.agentJobs.RunsForTask(r.Context(), identity(r).TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]agentJobRunsJSON, 0, len(jobs))
	for _, j := range jobs {
		runs := make([]agentRunJSON, 0, len(j.Runs))
		for _, run := range j.Runs {
			a := agentRunJSON{Turn: run.Turn, Model: run.Model, ToolArgs: rawOrNull(run.ToolArgs), ToolResult: rawOrNull(run.ToolResult),
				Outcome: run.Outcome, PromptTokens: run.PromptTokens, CompletionTokens: run.CompletionTokens,
				LatencyMS: run.LatencyMS, CreatedAt: rfc3339(run.CreatedAt)}
			if run.ToolName != "" {
				name := run.ToolName
				a.ToolName = &name
			}
			runs = append(runs, a)
		}
		out = append(out, agentJobRunsJSON{Job: toAgentJobJSON(j.Job), Runs: runs})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_runs": out})
}

type outboxEntryJSON struct {
	ID             uuid.UUID       `json:"id"`
	AggregateID    uuid.UUID       `json:"aggregate_id"`
	EventType      string          `json:"event_type"`
	IdempotencyKey string          `json:"idempotency_key"`
	Status         string          `json:"status"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	NextAttemptAt  string          `json:"next_attempt_at"`
	LastError      *string         `json:"last_error"`
	CreatedAt      string          `json:"created_at"`
	DispatchedAt   *string         `json:"dispatched_at"`
	Payload        json.RawMessage `json:"payload"`
}

func toOutboxEntryJSON(e domain.OutboxEntry) outboxEntryJSON {
	out := outboxEntryJSON{ID: e.ID, AggregateID: e.AggregateID, EventType: e.EventType, IdempotencyKey: e.IdempotencyKey,
		Status: string(e.Status), Attempts: e.Attempts, MaxAttempts: e.MaxAttempts, NextAttemptAt: rfc3339(e.NextAttemptAt),
		CreatedAt: rfc3339(e.CreatedAt), Payload: e.Payload}
	if len(out.Payload) == 0 {
		out.Payload = json.RawMessage(`{}`)
	}
	if e.LastError != "" {
		msg := e.LastError
		out.LastError = &msg
	}
	if e.DispatchedAt != nil {
		at := rfc3339(*e.DispatchedAt)
		out.DispatchedAt = &at
	}
	return out
}

type outboxListJSON struct {
	Entries    []outboxEntryJSON `json:"entries"`
	NextCursor *string           `json:"next_cursor"`
}

// listOutbox serves GET /admin/outbox: the tenant's rows, newest first,
// optionally of one status.
func (h *handlers) listOutbox(w http.ResponseWriter, r *http.Request) {
	limit, after, err := pageParams(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.outbox.List(r.Context(), repository.OutboxFilter{TenantID: identity(r).TenantID,
		Status: domain.OutboxStatus(r.URL.Query().Get("status")), Limit: limit, After: listCursor(after)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := outboxListJSON{Entries: make([]outboxEntryJSON, 0, len(page.Entries)), NextCursor: encodeListCursor(page.Next)}
	for _, e := range page.Entries {
		out.Entries = append(out.Entries, toOutboxEntryJSON(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// replayOutbox serves POST /admin/outbox/{id}/replay: a dead-letter row goes
// back to pending, and the relay sends it again.
func (h *handlers) replayOutbox(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, domain.ErrNotFound)
		return
	}
	e, err := h.outbox.Replay(r.Context(), identity(r).TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "outbox row replayed", "outbox_id", id, "by", identity(r).UserID)
	writeJSON(w, http.StatusOK, toOutboxEntryJSON(e))
}
