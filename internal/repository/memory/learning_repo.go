package memory

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// LearningRepo implements repository.LearningSuggestionRepository.
type LearningRepo struct{ st *Store }

// Learning returns the learning suggestion repository.
func (st *Store) Learning() *LearningRepo { return &LearningRepo{st} }

var _ repository.LearningSuggestionRepository = (*LearningRepo)(nil)

func (r *LearningRepo) Insert(ctx context.Context, s domain.LearningSuggestion) (bool, error) {
	inserted := false
	err := r.st.do(ctx, func(st *state) error {
		if _, ok := st.jobs[s.JobID]; !ok {
			return domain.ErrNotFound // the foreign key to agent_jobs
		}
		for _, have := range st.learning {
			if have.JobID == s.JobID && have.Pattern.Equal(s.Pattern) {
				return nil
			}
		}
		s.CreatedAt = micro(s.CreatedAt)
		s.Pattern.Label = cloneStr(s.Pattern.Label)
		st.learning = append(st.learning, s)
		inserted = true
		return nil
	})
	return inserted, err
}

func (r *LearningRepo) PatternsSince(ctx context.Context, tenantID string, since time.Time) ([]domain.EventPattern, error) {
	out := []domain.EventPattern{}
	err := r.st.do(ctx, func(st *state) error {
		for _, s := range st.learning {
			if s.TenantID == tenantID && !s.CreatedAt.Before(since) && !slices.ContainsFunc(out, s.Pattern.Equal) {
				out = append(out, domain.EventPattern{EventType: s.Pattern.EventType, Label: cloneStr(s.Pattern.Label)})
			}
		}
		return nil
	})
	slices.SortFunc(out, func(a, b domain.EventPattern) int {
		la, lb := "", ""
		if a.Label != nil {
			la = "\x01" + *a.Label
		}
		if b.Label != nil {
			lb = "\x01" + *b.Label
		}
		return cmp.Or(strings.Compare(a.EventType, b.EventType), strings.Compare(la, lb))
	})
	return out, err
}

// ListLearningTenants mirrors the PostgreSQL query.
func (r *WorkspaceRepo) ListLearningTenants(ctx context.Context, week string, limit int) ([]domain.LearningTenant, error) {
	first := map[string]string{}
	err := r.st.do(ctx, func(s *state) error {
		for k, w := range s.workspaces {
			if !w.AgentSettings.Allows(domain.AgentLearning) {
				continue
			}
			if _, queued := s.jobKeys[domain.LearningJobDedupKey(k.tenant, week)]; queued {
				continue
			}
			if cur, seen := first[k.tenant]; !seen || k.id < cur {
				first[k.tenant] = k.id
			}
		}
		return nil
	})
	out := make([]domain.LearningTenant, 0, len(first))
	for tenant, ws := range first {
		out = append(out, domain.LearningTenant{TenantID: tenant, WorkspaceID: ws})
	}
	slices.SortFunc(out, func(a, b domain.LearningTenant) int { return strings.Compare(a.TenantID, b.TenantID) })
	return out[:min(limit, len(out))], err
}
