package api

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/service"
)

// LearningClient is the part of the AgentPulse client the admin learning
// endpoints use.
type LearningClient interface {
	LearningSuggestions(ctx context.Context, tenantID string) ([]agentpulse.Suggestion, error)
	ReviewLearningSuggestion(ctx context.Context, tenantID, id, decision, decidedBy string) (agentpulse.Suggestion, error)
}

// proxyTimeout bounds each proxied call to AgentPulse.
const proxyTimeout = 5 * time.Second

func (h *handlers) adminRoutes(r chi.Router) {
	r.Use(adminOnly(h))
	r.Post("/workspaces", h.createWorkspace)
	r.Get("/workspaces/{id}/settings", h.getWorkspace)
	r.Patch("/workspaces/{id}/settings", h.updateWorkspace)
	r.Post("/workspaces/{id}/bootstrap-rules", h.bootstrapRules)
	r.Get("/admin/learning/suggestions", h.listSuggestions)
	r.Post("/admin/learning/suggestions/{id}/decision", h.decideSuggestion)
	if h.agentJobs != nil {
		r.Get("/admin/agent-jobs/{id}/runs", h.getAgentJobRuns)
	}
	if h.outbox != nil {
		r.Get("/admin/outbox", h.listOutbox)
		r.Post("/admin/outbox/{id}/replay", h.replayOutbox)
	}
}

func adminOnly(h *handlers) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := requireAdmin(r); err != nil {
				h.fail(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type workspaceJSON struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	AgentSettings agentSettingsJSON `json:"agent_settings"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

type agentSettingsJSON struct {
	AgentsEnabled               bool     `json:"agents_enabled"`
	DisabledAgents              []string `json:"disabled_agents"`
	RequireHITLForAll           bool     `json:"require_hitl_for_all"`
	MaxSubtasksPerDecomposition int      `json:"max_subtasks_per_decomposition"`
}

type agentSettingsPatchJSON struct {
	AgentsEnabled               *bool     `json:"agents_enabled"`
	DisabledAgents              *[]string `json:"disabled_agents"`
	RequireHITLForAll           *bool     `json:"require_hitl_for_all"`
	MaxSubtasksPerDecomposition *int      `json:"max_subtasks_per_decomposition"`
}

type ruleJSON struct {
	Key              string    `json:"key"`
	AgentPulseRuleID string    `json:"agentpulse_rule_id"`
	CreatedAt        time.Time `json:"created_at"`
}

func toWorkspaceJSON(w domain.Workspace) workspaceJSON {
	s := w.AgentSettings
	disabled := s.DisabledAgents
	if disabled == nil {
		disabled = []string{}
	}
	return workspaceJSON{
		ID: w.ID, Name: w.Name, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
		AgentSettings: agentSettingsJSON{
			AgentsEnabled:               s.AgentsEnabled,
			DisabledAgents:              disabled,
			RequireHITLForAll:           s.RequireHITLForAll,
			MaxSubtasksPerDecomposition: s.MaxSubtasksPerDecomposition,
		},
	}
}

func toRulesJSON(rules []domain.WorkspaceRule) []ruleJSON {
	out := make([]ruleJSON, 0, len(rules))
	for _, r := range rules {
		out = append(out, ruleJSON{Key: r.Key, AgentPulseRuleID: r.AgentPulseRuleID, CreatedAt: r.CreatedAt})
	}
	return out
}

type createWorkspaceRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type createWorkspaceResponse struct {
	workspaceJSON
	Rules             []ruleJSON `json:"rules"`
	RulesBootstrapped bool       `json:"rules_bootstrapped"`
	RulesError        string     `json:"rules_error,omitempty"`
}

// createWorkspace creates the workspace even when AgentPulse is unreachable
// (default G9). The response then says so, and bootstrap-rules can be
// called again.
func (h *handlers) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var req createWorkspaceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	ws, rules, rulesErr, err := h.workspaces.Create(r.Context(), identity(r).TenantID, req.ID, req.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := createWorkspaceResponse{workspaceJSON: toWorkspaceJSON(ws), Rules: toRulesJSON(rules), RulesBootstrapped: rulesErr == nil}
	if rulesErr != nil {
		h.logger.WarnContext(r.Context(), "workspace created without its AgentPulse rules", "workspace_id", ws.ID, "error", rulesErr)
		out.RulesError = classify(rulesErr).message
	}
	w.Header().Set("Location", "/api/v1/workspaces/"+ws.ID+"/settings")
	writeJSON(w, http.StatusCreated, out)
}

func (h *handlers) getWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := h.workspaces.Get(r.Context(), identity(r).TenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toWorkspaceJSON(ws))
}

type updateWorkspaceRequest struct {
	Name          *string                 `json:"name"`
	AgentSettings *agentSettingsPatchJSON `json:"agent_settings"`
}

func (h *handlers) updateWorkspace(w http.ResponseWriter, r *http.Request) {
	var req updateWorkspaceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	p := service.WorkspaceSettingsPatch{Name: req.Name}
	if a := req.AgentSettings; a != nil {
		p.AgentSettings = &domain.AgentSettingsPatch{
			AgentsEnabled:               a.AgentsEnabled,
			DisabledAgents:              a.DisabledAgents,
			RequireHITLForAll:           a.RequireHITLForAll,
			MaxSubtasksPerDecomposition: a.MaxSubtasksPerDecomposition,
		}
	}
	ws, err := h.workspaces.UpdateSettings(r.Context(), identity(r).TenantID, chi.URLParam(r, "id"), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toWorkspaceJSON(ws))
}

func (h *handlers) bootstrapRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.workspaces.BootstrapRules(r.Context(), identity(r).TenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": toRulesJSON(rules)})
}

// learningSuggestionJSON is an AgentPulse learning suggestion in Taskify's
// snake_case (rule 9). Only the fields the contract lists are copied, so a
// field AgentPulse adds does not reach clients until the contract has it.
type learningSuggestionJSON struct {
	ID                 string            `json:"id"`
	Status             string            `json:"status"`
	RuleID             *string           `json:"rule_id"`
	EventPattern       *eventPatternJSON `json:"event_pattern"`
	CurrentAdjustment  int               `json:"current_adjustment"`
	ProposedAdjustment int               `json:"proposed_adjustment"`
	SignalCount        int               `json:"signal_count"`
	Rationale          string            `json:"rationale"`
}

type eventPatternJSON struct {
	EventType string  `json:"event_type"`
	Label     *string `json:"label"`
}

func toLearningSuggestionJSON(s agentpulse.Suggestion) learningSuggestionJSON {
	b := s.Body
	out := learningSuggestionJSON{ID: s.ID, Status: s.Status, CurrentAdjustment: b.CurrentAdjustment,
		ProposedAdjustment: b.ProposedAdjustment, SignalCount: b.SignalCount, Rationale: b.Rationale}
	if b.RuleID != "" {
		id := b.RuleID
		out.RuleID = &id
	}
	if p := b.EventPattern; p != nil {
		out.EventPattern = &eventPatternJSON{EventType: p.EventType, Label: p.Label}
	}
	return out
}

// listSuggestions serves GET /admin/learning/suggestions: the tenant's
// suggestions, read from AgentPulse.
func (h *handlers) listSuggestions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), proxyTimeout)
	defer cancel()
	list, err := h.learning.LearningSuggestions(ctx, identity(r).TenantID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]learningSuggestionJSON, 0, len(list))
	for _, s := range list {
		out = append(out, toLearningSuggestionJSON(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": out})
}

type decisionRequest struct {
	Decision string `json:"decision"`
}

func (h *handlers) decideSuggestion(w http.ResponseWriter, r *http.Request) {
	var req decisionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.Decision != "approve" && req.Decision != "reject" {
		h.fail(w, r, newError(http.StatusUnprocessableEntity, CodeValidation, "decision must be approve or reject"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), proxyTimeout)
	defer cancel()
	// The call names the caller's tenant, and AgentPulse answers 404 for
	// another tenant's suggestion (AP-12), so an admin decides only their
	// own tenant's suggestions.
	decided, err := h.learning.ReviewLearningSuggestion(ctx, identity(r).TenantID, chi.URLParam(r, "id"), req.Decision, identity(r).UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toLearningSuggestionJSON(decided))
}
