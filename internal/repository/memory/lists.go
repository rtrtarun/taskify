package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// The list reads the clients need (M12-04).

func (r *TaskRepo) ListChildren(ctx context.Context, tenantID string, parentID uuid.UUID) ([]*domain.Task, error) {
	out := []*domain.Task{}
	err := r.st.do(ctx, func(s *state) error {
		for _, t := range s.tasks {
			if t.TenantID == tenantID && t.ParentID != nil && *t.ParentID == parentID {
				out = append(out, copyTask(t))
			}
		}
		return nil
	})
	slices.SortFunc(out, func(a, b *domain.Task) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID.String(), b.ID.String())
	})
	return out, err
}

// newestFirst orders by (created_at, id) descending.
func newestFirst(aAt time.Time, aID uuid.UUID, bAt time.Time, bID uuid.UUID) int {
	if c := bAt.Compare(aAt); c != 0 {
		return c
	}
	return strings.Compare(bID.String(), aID.String())
}

// afterCursor reports whether (at, id) sorts after the cursor in descending
// order, that is (at, id) < (cursor.CreatedAt, cursor.ID).
func afterCursor(at time.Time, id uuid.UUID, c *repository.ListCursor) bool {
	if c == nil {
		return true
	}
	if cmp := at.Compare(c.CreatedAt); cmp != 0 {
		return cmp < 0
	}
	return strings.Compare(id.String(), c.ID.String()) < 0
}

func (r *AgentJobRepo) List(ctx context.Context, f repository.AgentJobFilter) (page repository.AgentJobPage, err error) {
	if f.Limit <= 0 {
		return page, fmt.Errorf("%w: limit must be positive", domain.ErrValidation)
	}
	var matched []*domain.AgentJob
	err = r.st.do(ctx, func(s *state) error {
		for _, j := range s.jobs {
			switch {
			case j.TenantID != f.TenantID,
				f.Status != "" && j.Status != f.Status,
				f.AgentType != "" && j.AgentType != f.AgentType,
				f.TaskID != nil && (j.TaskID == nil || *j.TaskID != *f.TaskID),
				!afterCursor(j.CreatedAt, j.ID, f.After):
				continue
			}
			matched = append(matched, copyJob(j))
		}
		return nil
	})
	slices.SortFunc(matched, func(a, b *domain.AgentJob) int { return newestFirst(a.CreatedAt, a.ID, b.CreatedAt, b.ID) })
	page.Jobs = []*domain.AgentJob{}
	for i, j := range matched {
		if i == f.Limit {
			last := page.Jobs[f.Limit-1]
			page.Next = &repository.ListCursor{CreatedAt: last.CreatedAt, ID: last.ID}
			break
		}
		page.Jobs = append(page.Jobs, j)
	}
	return page, err
}

func (r *OutboxRepo) List(ctx context.Context, f repository.OutboxFilter) (page repository.OutboxPage, err error) {
	if f.Limit <= 0 {
		return page, fmt.Errorf("%w: limit must be positive", domain.ErrValidation)
	}
	var matched []domain.OutboxEntry
	err = r.st.do(ctx, func(s *state) error {
		for _, e := range s.outbox {
			if e.TenantID != f.TenantID || (f.Status != "" && e.Status != f.Status) || !afterCursor(e.CreatedAt, e.ID, f.After) {
				continue
			}
			matched = append(matched, copyEntry(e))
		}
		return nil
	})
	slices.SortFunc(matched, func(a, b domain.OutboxEntry) int { return newestFirst(a.CreatedAt, a.ID, b.CreatedAt, b.ID) })
	page.Entries = []domain.OutboxEntry{}
	for i, e := range matched {
		if i == f.Limit {
			last := page.Entries[f.Limit-1]
			page.Next = &repository.ListCursor{CreatedAt: last.CreatedAt, ID: last.ID}
			break
		}
		page.Entries = append(page.Entries, e)
	}
	return page, err
}

func (r *OutboxRepo) Replay(ctx context.Context, tenantID string, id uuid.UUID, now time.Time) (out domain.OutboxEntry, err error) {
	err = r.st.do(ctx, func(s *state) error {
		e, ok := s.outbox[id]
		switch {
		case !ok || e.TenantID != tenantID:
			return domain.ErrNotFound
		case e.Status != domain.OutboxDeadLetter:
			return domain.ErrNotDeadLetter
		}
		e.Status, e.Attempts, e.NextAttemptAt, e.ClaimedAt = domain.OutboxPending, 0, micro(now), nil
		out = copyEntry(e)
		return nil
	})
	return out, err
}

// copyEntry returns a copy sharing no slices or maps.
func copyEntry(e *domain.OutboxEntry) domain.OutboxEntry {
	c := *e
	c.Payload = slices.Clone(e.Payload)
	if e.TraceContext != nil {
		c.TraceContext = make(map[string]string, len(e.TraceContext))
		for k, v := range e.TraceContext {
			c.TraceContext[k] = v
		}
	}
	return c
}
