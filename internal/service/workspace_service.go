package service

import (
	"context"
	"fmt"
	"time"

	"github.com/rtrtarun/taskify/internal/agentpulse"
	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// RuleCreator registers a routing rule with AgentPulse.
type RuleCreator interface {
	CreateRule(ctx context.Context, r agentpulse.Rule) (string, error)
}

// agentPulseTimeout bounds each synchronous call to AgentPulse.
const agentPulseTimeout = 5 * time.Second

// WorkspaceService manages workspaces and their default AgentPulse rules.
type WorkspaceService struct {
	tx         repository.TxManager
	workspaces repository.WorkspaceRepository
	rules      repository.WorkspaceRuleRepository
	creator    RuleCreator
	now        func() time.Time
}

// NewWorkspaceService returns a WorkspaceService.
func NewWorkspaceService(tx repository.TxManager, ws repository.WorkspaceRepository, rules repository.WorkspaceRuleRepository, creator RuleCreator, now func() time.Time) *WorkspaceService {
	if now == nil {
		now = time.Now
	}
	return &WorkspaceService{tx: tx, workspaces: ws, rules: rules, creator: creator, now: now}
}

func (s *WorkspaceService) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// Create creates a workspace and then registers its default rules. The
// workspace is created even when AgentPulse cannot be reached; rulesErr then
// reports why, and BootstrapRules can be called again later.
func (s *WorkspaceService) Create(ctx context.Context, tenantID, id, name string) (w domain.Workspace, rules []domain.WorkspaceRule, rulesErr error, err error) {
	if w, err = domain.NewWorkspace(tenantID, id, name, s.clock()); err != nil {
		return w, nil, nil, err
	}
	if err = s.workspaces.Create(ctx, w); err != nil {
		return w, nil, nil, err
	}
	rules, rulesErr = s.BootstrapRules(ctx, tenantID, id)
	return w, rules, rulesErr, nil
}

// Get returns a workspace.
func (s *WorkspaceService) Get(ctx context.Context, tenantID, id string) (domain.Workspace, error) {
	return s.workspaces.Get(ctx, tenantID, id)
}

// Rename changes a workspace's name.
func (s *WorkspaceService) Rename(ctx context.Context, tenantID, id, name string) (domain.Workspace, error) {
	return s.UpdateSettings(ctx, tenantID, id, WorkspaceSettingsPatch{Name: &name})
}

// WorkspaceSettingsPatch changes some workspace settings; nil fields are
// left alone.
type WorkspaceSettingsPatch struct {
	Name          *string
	AgentSettings *domain.AgentSettingsPatch
}

// UpdateSettings applies the patch under a lock on the workspace row. Either
// every field changes or, on a validation error, none does.
func (s *WorkspaceService) UpdateSettings(ctx context.Context, tenantID, id string, p WorkspaceSettingsPatch) (domain.Workspace, error) {
	var w domain.Workspace
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		if w, err = s.workspaces.GetForUpdate(ctx, tenantID, id); err != nil {
			return err
		}
		if p.Name == nil && p.AgentSettings == nil {
			return nil
		}
		now := s.clock()
		if p.Name != nil {
			if err := w.Rename(*p.Name, now); err != nil {
				return err
			}
		}
		if p.AgentSettings != nil {
			if err := w.UpdateAgentSettings(*p.AgentSettings, now); err != nil {
				return err
			}
		}
		return s.workspaces.Update(ctx, w)
	})
	return w, err
}

// BootstrapRules registers whichever default rules the workspace does not
// have yet, and returns all of its rules. Calling it again never creates a
// duplicate: each rule is created under a lock on the workspace row and
// recorded before the lock is released, so a concurrent call waits and then
// sees it. A failure leaves the rules created so far recorded.
func (s *WorkspaceService) BootstrapRules(ctx context.Context, tenantID, workspaceID string) ([]domain.WorkspaceRule, error) {
	if _, err := s.workspaces.Get(ctx, tenantID, workspaceID); err != nil {
		return nil, err
	}
	for _, d := range DefaultRules(tenantID, workspaceID) {
		err := s.tx.WithTx(ctx, func(ctx context.Context) error {
			if _, err := s.workspaces.GetForUpdate(ctx, tenantID, workspaceID); err != nil {
				return err
			}
			existing, err := s.rules.List(ctx, tenantID, workspaceID)
			if err != nil {
				return err
			}
			for _, r := range existing {
				if r.Key == d.Key {
					return nil
				}
			}
			callCtx, cancel := context.WithTimeout(ctx, agentPulseTimeout)
			defer cancel()
			id, err := s.creator.CreateRule(callCtx, d.Rule)
			if err != nil {
				return fmt.Errorf("register rule %s: %w", d.Key, err)
			}
			return s.rules.Create(ctx, domain.WorkspaceRule{
				TenantID: tenantID, WorkspaceID: workspaceID, Key: d.Key,
				AgentPulseRuleID: id, CreatedAt: s.clock(),
			})
		})
		if err != nil {
			return nil, err
		}
	}
	return s.rules.List(ctx, tenantID, workspaceID)
}

// DefaultRule is one of the rules every workspace gets.
type DefaultRule struct {
	Key  string
	Rule agentpulse.Rule
}

// DefaultRules returns the three rules from docs/archive/initial.md section 4.C.3,
// scoped to one workspace through payload.workspaceId.
//
// Changes from the source: the immediate-escalation rule matches manual
// blocks only, since dependency blocks are applied by the system and are
// meant to be quiet (docs/architecture.md, decision 23); and it is a critical
// rule that also matches task.escalated (AP-03), so p1_critical tasks, manual
// blocks, and blocker escalations break through quiet hours. Every entry of
// the critical rule has a real condition besides the workspace, as AgentPulse
// requires.
func DefaultRules(tenantID, workspaceID string) []DefaultRule {
	in := func(extra map[string]any) map[string]any {
		m := map[string]any{"workspaceId": workspaceID}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	name := func(key string) string { return "taskify:" + workspaceID + ":" + key }
	return []DefaultRule{
		{Key: "critical-or-blocked", Rule: agentpulse.Rule{
			TenantID: tenantID, Name: name("critical-or-blocked"), AttentionClass: agentpulse.AttentionCritical,
			MatchAny: []agentpulse.RuleMatch{
				{PayloadMatch: in(map[string]any{"priority": string(domain.PriorityCritical)})},
				{PayloadMatch: in(map[string]any{"status": string(domain.StatusBlocked), "blockedReason": string(domain.BlockedManual)})},
				{EventTypes: []string{domain.EventTaskEscalated}, PayloadMatch: in(nil)},
			},
			Channels:     []string{"slack", "email", "in_app"},
			BypassDigest: true,
		}},
		{Key: "assignments-and-mentions", Rule: agentpulse.Rule{
			TenantID: tenantID, Name: name("assignments-and-mentions"),
			MatchAny: []agentpulse.RuleMatch{
				{EventTypes: []string{domain.EventTaskAssigned}, PayloadMatch: in(nil)},
				{EventTypes: []string{domain.EventTaskCommentAdded}, PayloadMatch: in(map[string]any{"isMentioned": true})},
			},
			Channels: []string{"in_app", "email"},
		}},
		{Key: "low-priority-digest", Rule: agentpulse.Rule{
			TenantID: tenantID, Name: name("low-priority-digest"),
			MatchAny: []agentpulse.RuleMatch{
				{PayloadMatch: in(map[string]any{"priority": string(domain.PriorityLow)})},
			},
			Channels: []string{"digest"},
		}},
	}
}
