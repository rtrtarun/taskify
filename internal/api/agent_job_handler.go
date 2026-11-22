package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/service"
)

// Agent job error codes, all with status 409.
const (
	CodeStaleVersion        = "stale_version"
	CodeAlreadyDecided      = "already_decided"
	CodeNotAwaitingApproval = "not_awaiting_approval"
)

func (h *handlers) agentJobRoutes(r chi.Router) {
	r.Get("/", h.listAgentJobs)
	r.Route("/{id}", func(r chi.Router) {
		r.Get("/", h.getAgentJob)
		r.Post("/apply", h.applyAgentJob)
		r.Post("/dismiss", h.dismissAgentJob)
	})
}

type agentJobJSON struct {
	ID              string          `json:"id"`
	TaskID          *string         `json:"task_id"`
	AgentType       string          `json:"agent_type"`
	Status          string          `json:"status"`
	BaseTaskVersion *int            `json:"base_task_version"`
	ProposedActions json.RawMessage `json:"proposed_actions"`
	ExecutionResult json.RawMessage `json:"execution_result"`
	ErrorMessage    *string         `json:"error_message"`
	Attempts        int             `json:"attempts"`
	DecidedBy       *string         `json:"decided_by"`
	DecidedAt       *string         `json:"decided_at"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
}

// agentJobDecisionJSON answers apply and dismiss. changed is false when the
// same decision had already been made.
type agentJobDecisionJSON struct {
	Job     agentJobJSON `json:"job"`
	Changed bool         `json:"changed"`
}

func toAgentJobJSON(j *domain.AgentJob) agentJobJSON {
	out := agentJobJSON{
		ID: j.ID.String(), AgentType: j.AgentType, Status: string(j.Status), BaseTaskVersion: j.BaseTaskVersion,
		ProposedActions: rawOrNull(j.ProposedActions), ExecutionResult: rawOrNull(j.ExecutionResult),
		Attempts: j.Attempts, CreatedAt: rfc3339(j.CreatedAt), UpdatedAt: rfc3339(j.UpdatedAt),
	}
	if j.TaskID != nil {
		s := j.TaskID.String()
		out.TaskID = &s
	}
	if j.ErrorMessage != "" {
		out.ErrorMessage = &j.ErrorMessage
	}
	if j.DecidedBy != "" {
		out.DecidedBy = &j.DecidedBy
	}
	if j.DecidedAt != nil {
		s := rfc3339(*j.DecidedAt)
		out.DecidedAt = &s
	}
	return out
}

func rawOrNull(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// briefJSON is the caller's morning brief: the stored report, flat, with
// the job that wrote it.
type briefJSON struct {
	JobID     string `json:"job_id"`
	WrittenAt string `json:"written_at"`
	domain.MorningBrief
}

// getMyBrief serves GET /me/brief (G15, M10-03): the caller's most recent
// morning brief, for the frontend (M14-10). Tenant and user come from the
// token only, so nobody can read another person's brief.
func (h *handlers) getMyBrief(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	b, err := h.agentJobs.LatestBrief(r.Context(), id.TenantID, id.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, briefJSON{JobID: b.JobID.String(), WrittenAt: rfc3339(b.WrittenAt), MorningBrief: b.MorningBrief})
}

func decider(r *http.Request) service.Decider {
	id := identity(r)
	return service.Decider{UserID: id.UserID, Admin: id.IsAdmin()}
}

func (h *handlers) getAgentJob(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r) // the same rule: a malformed ID names nothing
	if err != nil {
		h.fail(w, r, err)
		return
	}
	job, err := h.agentJobs.Get(r.Context(), identity(r).TenantID, id, decider(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAgentJobJSON(job))
}

func (h *handlers) applyAgentJob(w http.ResponseWriter, r *http.Request) {
	h.decideAgentJob(w, r, h.agentJobs.Apply)
}

func (h *handlers) dismissAgentJob(w http.ResponseWriter, r *http.Request) {
	h.decideAgentJob(w, r, h.agentJobs.Dismiss)
}

type decideFunc func(ctx context.Context, tenantID string, id uuid.UUID, d service.Decider) (*domain.AgentJob, bool, error)

func (h *handlers) decideAgentJob(w http.ResponseWriter, r *http.Request, decide decideFunc) {
	id, err := taskID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	job, changed, err := decide(r.Context(), identity(r).TenantID, id, decider(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, agentJobDecisionJSON{Job: toAgentJobJSON(job), Changed: changed})
}
