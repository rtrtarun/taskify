package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// DependencyRepo implements repository.DependencyRepository.
type DependencyRepo struct{ st *Store }

// Dependencies returns the dependency repository.
func (st *Store) Dependencies() *DependencyRepo { return &DependencyRepo{st} }

var _ repository.DependencyRepository = (*DependencyRepo)(nil)

// LockTenant needs no work: memory transactions are already serialised.
func (r *DependencyRepo) LockTenant(ctx context.Context, _ string) error {
	if !inTx(ctx) {
		return repository.ErrTxRequired
	}
	return nil
}

func (r *DependencyRepo) Create(ctx context.Context, d domain.Dependency) (created bool, err error) {
	err = r.st.do(ctx, func(s *state) error {
		// The same checks the schema makes.
		if d.TaskID == d.DependsOnTaskID {
			return fmt.Errorf("%w: a task cannot depend on itself", domain.ErrValidation)
		}
		if s.tasks[d.TaskID] == nil || s.tasks[d.DependsOnTaskID] == nil {
			return fmt.Errorf("%w: dependency on a missing task", domain.ErrValidation)
		}
		k := depKey{d.TaskID, d.DependsOnTaskID}
		if _, ok := s.deps[k]; ok {
			return nil
		}
		d.CreatedAt = micro(d.CreatedAt)
		s.deps[k] = d
		created = true
		return nil
	})
	return created, err
}

func (r *DependencyRepo) Reaches(ctx context.Context, tenantID string, from, to uuid.UUID) (found bool, err error) {
	err = r.st.do(ctx, func(s *state) error {
		seen := map[uuid.UUID]bool{}
		queue := []uuid.UUID{from}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for k, d := range s.deps {
				if k.task != cur || d.TenantID != tenantID || seen[k.dependsOn] {
					continue
				}
				if k.dependsOn == to {
					found = true
					return nil
				}
				seen[k.dependsOn] = true
				queue = append(queue, k.dependsOn)
			}
		}
		return nil
	})
	return found, err
}

func (r *DependencyRepo) ListBlockers(ctx context.Context, tenantID string, taskID uuid.UUID) ([]uuid.UUID, error) {
	return r.collect(ctx, tenantID, func(k depKey) (uuid.UUID, bool) { return k.dependsOn, k.task == taskID })
}

func (r *DependencyRepo) ListDependents(ctx context.Context, tenantID string, blockerID uuid.UUID) ([]uuid.UUID, error) {
	return r.collect(ctx, tenantID, func(k depKey) (uuid.UUID, bool) { return k.task, k.dependsOn == blockerID })
}

func (r *DependencyRepo) HasOpenBlocker(ctx context.Context, tenantID string, taskID uuid.UUID) (open bool, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for k, d := range s.deps {
			if k.task != taskID || d.TenantID != tenantID {
				continue
			}
			if b := s.tasks[k.dependsOn]; b != nil && !b.ResolvesDependencies() {
				open = true
				return nil
			}
		}
		return nil
	})
	return open, err
}

// collect returns the IDs pick selects, in the order PostgreSQL's uuid
// comparison gives.
func (r *DependencyRepo) collect(ctx context.Context, tenantID string, pick func(depKey) (uuid.UUID, bool)) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	err := r.st.do(ctx, func(s *state) error {
		for k, d := range s.deps {
			if id, ok := pick(k); ok && d.TenantID == tenantID {
				out = append(out, id)
			}
		}
		return nil
	})
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out, err
}
