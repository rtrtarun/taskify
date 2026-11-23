package memory

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// TaskRepo implements repository.TaskRepository.
type TaskRepo struct{ st *Store }

// WorkspaceRepo implements repository.WorkspaceRepository.
type WorkspaceRepo struct{ st *Store }

// CommentRepo implements repository.CommentRepository.
type CommentRepo struct{ st *Store }

// OutboxRepo implements repository.OutboxRepository.
type OutboxRepo struct{ st *Store }

// Tasks returns the task repository.
func (st *Store) Tasks() *TaskRepo { return &TaskRepo{st} }

// Workspaces returns the workspace repository.
func (st *Store) Workspaces() *WorkspaceRepo { return &WorkspaceRepo{st} }

// Comments returns the comment repository.
func (st *Store) Comments() *CommentRepo { return &CommentRepo{st} }

// Outbox returns the outbox repository.
func (st *Store) Outbox() *OutboxRepo { return &OutboxRepo{st} }

// Rules returns the workspace rule repository.
func (st *Store) Rules() *RuleRepo { return &RuleRepo{st} }

// Deliveries returns the webhook delivery repository.
func (st *Store) Deliveries() *DeliveryRepo { return &DeliveryRepo{st} }

// RuleRepo implements repository.WorkspaceRuleRepository.
type RuleRepo struct{ st *Store }

// DeliveryRepo implements repository.WebhookDeliveryRepository.
type DeliveryRepo struct{ st *Store }

var (
	_ repository.TaskRepository      = (*TaskRepo)(nil)
	_ repository.WorkspaceRepository = (*WorkspaceRepo)(nil)
	_ repository.CommentRepository   = (*CommentRepo)(nil)
	_ repository.OutboxRepository    = (*OutboxRepo)(nil)

	_ repository.WorkspaceRuleRepository   = (*RuleRepo)(nil)
	_ repository.WebhookDeliveryRepository = (*DeliveryRepo)(nil)
)

// micro matches PostgreSQL's timestamp precision, so both stores return the
// same values.
func micro(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func microPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := micro(*t)
	return &v
}

func normalizeTimes(t *domain.Task) {
	t.CreatedAt, t.UpdatedAt = micro(t.CreatedAt), micro(t.UpdatedAt)
	t.DueAt, t.RemindAt = microPtr(t.DueAt), microPtr(t.RemindAt)
	t.CompletedAt, t.ArchivedAt = microPtr(t.CompletedAt), microPtr(t.ArchivedAt)
	t.LastDueSoonNotifiedAt, t.LastOverdueNotifiedAt = microPtr(t.LastDueSoonNotifiedAt), microPtr(t.LastOverdueNotifiedAt)
	t.UnblockedAt = microPtr(t.UnblockedAt)
	slices.Sort(t.Watchers)
}

// --- workspaces ------------------------------------------------------------

func (r *WorkspaceRepo) Create(ctx context.Context, w domain.Workspace) error {
	return r.st.do(ctx, func(s *state) error {
		k := wsKey{w.TenantID, w.ID}
		if _, ok := s.workspaces[k]; ok {
			return fmt.Errorf("%w: workspace", domain.ErrAlreadyExists)
		}
		w.CreatedAt, w.UpdatedAt = micro(w.CreatedAt), micro(w.UpdatedAt)
		w.AgentSettings = cloneSettings(w.AgentSettings)
		s.workspaces[k] = w
		return nil
	})
}

// cloneSettings copies settings so stored state never shares a slice with a
// caller.
func cloneSettings(a domain.AgentSettings) domain.AgentSettings {
	a.DisabledAgents = append([]string{}, a.DisabledAgents...)
	return a
}

func (r *WorkspaceRepo) Get(ctx context.Context, tenantID, id string) (w domain.Workspace, err error) {
	err = r.st.do(ctx, func(s *state) error {
		var ok bool
		if w, ok = s.workspaces[wsKey{tenantID, id}]; !ok {
			return domain.ErrNotFound
		}
		w.AgentSettings = cloneSettings(w.AgentSettings)
		return nil
	})
	return w, err
}

func (r *WorkspaceRepo) GetForUpdate(ctx context.Context, tenantID, id string) (domain.Workspace, error) {
	if !inTx(ctx) {
		return domain.Workspace{}, repository.ErrTxRequired
	}
	return r.Get(ctx, tenantID, id)
}

func (r *WorkspaceRepo) Update(ctx context.Context, w domain.Workspace) error {
	return r.st.do(ctx, func(s *state) error {
		k := wsKey{w.TenantID, w.ID}
		stored, ok := s.workspaces[k]
		if !ok {
			return domain.ErrNotFound
		}
		stored.Name, stored.UpdatedAt = w.Name, micro(w.UpdatedAt)
		stored.AgentSettings = cloneSettings(w.AgentSettings)
		s.workspaces[k] = stored
		return nil
	})
}

func (r *WorkspaceRepo) List(ctx context.Context, tenantID, after string, limit int) (out []domain.Workspace, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for k, w := range s.workspaces {
			if k.tenant == tenantID && (after == "" || k.id > after) {
				w.AgentSettings = cloneSettings(w.AgentSettings)
				out = append(out, w)
			}
		}
		slices.SortFunc(out, func(a, b domain.Workspace) int { return strings.Compare(a.ID, b.ID) })
		if len(out) > limit {
			out = out[:limit]
		}
		return nil
	})
	return out, err
}

// --- workspace rules and webhook deliveries --------------------------------

func (r *RuleRepo) List(ctx context.Context, tenantID, workspaceID string) (out []domain.WorkspaceRule, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for k, v := range s.rules {
			if k.tenant == tenantID && k.workspace == workspaceID {
				out = append(out, v)
			}
		}
		slices.SortFunc(out, func(a, b domain.WorkspaceRule) int { return strings.Compare(a.Key, b.Key) })
		return nil
	})
	return out, err
}

func (r *RuleRepo) Create(ctx context.Context, w domain.WorkspaceRule) error {
	return r.st.do(ctx, func(s *state) error {
		if _, ok := s.workspaces[wsKey{w.TenantID, w.WorkspaceID}]; !ok {
			return fmt.Errorf("%w: workspace does not exist", domain.ErrValidation)
		}
		k := ruleKey{w.TenantID, w.WorkspaceID, w.Key}
		if _, ok := s.rules[k]; ok {
			return fmt.Errorf("%w: workspace rule", domain.ErrAlreadyExists)
		}
		w.CreatedAt = micro(w.CreatedAt)
		s.rules[k] = w
		return nil
	})
}

func (r *DeliveryRepo) Record(ctx context.Context, key string, at time.Time) (fresh bool, err error) {
	err = r.st.do(ctx, func(s *state) error {
		if _, seen := s.deliveries[key]; seen {
			return nil
		}
		s.deliveries[key] = micro(at)
		fresh = true
		return nil
	})
	return fresh, err
}

func (r *DeliveryRepo) DeleteBefore(ctx context.Context, cutoff time.Time) (n int, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for k, at := range s.deliveries {
			if at.Before(cutoff) {
				delete(s.deliveries, k)
				n++
			}
		}
		return nil
	})
	return n, err
}

// callbackKey is a tenant's AgentPulse callbackId.
type callbackKey struct{ tenant, id string }

func (r *DeliveryRepo) RecordCallback(ctx context.Context, tenantID, callbackID string, at time.Time) (fresh bool, err error) {
	err = r.st.do(ctx, func(s *state) error {
		k := callbackKey{tenantID, callbackID}
		if _, seen := s.callbacks[k]; seen {
			return nil
		}
		s.callbacks[k] = micro(at)
		fresh = true
		return nil
	})
	return fresh, err
}

func (r *DeliveryRepo) CallbackSeen(ctx context.Context, tenantID, callbackID string) (seen bool, err error) {
	err = r.st.do(ctx, func(s *state) error {
		_, seen = s.callbacks[callbackKey{tenantID, callbackID}]
		return nil
	})
	return seen, err
}

func (r *DeliveryRepo) DeleteCallbacksBefore(ctx context.Context, cutoff time.Time) (n int, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for k, at := range s.callbacks {
			if at.Before(cutoff) {
				delete(s.callbacks, k)
				n++
			}
		}
		return nil
	})
	return n, err
}

// --- tasks -----------------------------------------------------------------

func (r *TaskRepo) Create(ctx context.Context, t *domain.Task) error {
	return r.st.do(ctx, func(s *state) error {
		if _, ok := s.tasks[t.ID]; ok {
			return fmt.Errorf("%w: task", domain.ErrAlreadyExists)
		}
		// Mirror the database's foreign key and check constraint.
		if _, ok := s.workspaces[wsKey{t.TenantID, t.WorkspaceID}]; !ok {
			return fmt.Errorf("%w: workspace does not exist", domain.ErrValidation)
		}
		if err := checkBlocked(t); err != nil {
			return err
		}
		if p := t.ParentID; p != nil && (*p == t.ID || s.tasks[*p] == nil) {
			return fmt.Errorf("%w: parent task does not exist", domain.ErrValidation)
		}
		c := copyTask(t)
		normalizeTimes(c)
		s.tasks[t.ID] = c
		return nil
	})
}

func (r *TaskRepo) Get(ctx context.Context, tenantID string, id uuid.UUID) (t *domain.Task, err error) {
	err = r.st.do(ctx, func(s *state) error {
		stored, ok := s.tasks[id]
		if !ok || stored.TenantID != tenantID {
			return domain.ErrNotFound
		}
		t = copyTask(stored)
		return nil
	})
	return t, err
}

func (r *TaskRepo) GetForUpdate(ctx context.Context, tenantID string, id uuid.UUID) (*domain.Task, error) {
	if !inTx(ctx) {
		return nil, repository.ErrTxRequired
	}
	return r.Get(ctx, tenantID, id)
}

func (r *TaskRepo) Update(ctx context.Context, t *domain.Task, expectedVersion int) error {
	return r.st.do(ctx, func(s *state) error {
		stored, ok := s.tasks[t.ID]
		if !ok || stored.TenantID != t.TenantID {
			return domain.ErrNotFound
		}
		if stored.Version != expectedVersion {
			return domain.ErrVersionConflict
		}
		if err := checkBlocked(t); err != nil {
			return err
		}
		c := copyTask(t)
		// Immutable columns keep their stored values, as in the SQL update.
		c.WorkspaceID, c.CreatorID, c.CreatedAt = stored.WorkspaceID, stored.CreatorID, stored.CreatedAt
		c.ParentID = stored.ParentID
		normalizeTimes(c)
		s.tasks[t.ID] = c
		return nil
	})
}

// matches reports whether t belongs to the list f describes, ignoring its
// page cursor.
func matchesFilter(t *domain.Task, f repository.TaskFilter) bool {
	switch {
	case t.TenantID != f.TenantID,
		f.Status == "" && t.Status == domain.StatusArchived,
		f.Status != "" && t.Status != f.Status,
		f.AssigneeID != "" && t.AssigneeID != f.AssigneeID,
		f.Priority != "" && t.Priority != f.Priority,
		f.IDPrefix != "" && !strings.HasPrefix(t.ID.String(), strings.ToLower(f.IDPrefix)),
		f.UpdatedSince != nil && !t.UpdatedAt.After(*f.UpdatedSince):
		return false
	}
	return true
}

// archivedSince reports whether t was archived after f.UpdatedSince and fits
// the rest of f, except its status.
func archivedSince(t *domain.Task, f repository.TaskFilter) bool {
	return f.UpdatedSince != nil && t.TenantID == f.TenantID && t.Status == domain.StatusArchived &&
		t.ArchivedAt != nil && t.ArchivedAt.After(*f.UpdatedSince) &&
		(f.AssigneeID == "" || t.AssigneeID == f.AssigneeID) &&
		(f.Priority == "" || t.Priority == f.Priority) &&
		(f.IDPrefix == "" || strings.HasPrefix(t.ID.String(), strings.ToLower(f.IDPrefix)))
}

func (r *TaskRepo) ListState(ctx context.Context, f repository.TaskFilter) (st repository.ListState, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for _, t := range s.tasks {
			if matchesFilter(t, f) {
				st.Count++
				if t.UpdatedAt.After(st.Latest) {
					st.Latest = t.UpdatedAt
				}
			}
			if archivedSince(t, f) {
				st.ArchivedSince++
			}
		}
		return nil
	})
	return st, err
}

func (r *TaskRepo) ArchivedSince(ctx context.Context, f repository.TaskFilter) (ids []uuid.UUID, err error) {
	ids = []uuid.UUID{}
	err = r.st.do(ctx, func(s *state) error {
		var found []*domain.Task
		for _, t := range s.tasks {
			if archivedSince(t, f) {
				found = append(found, t)
			}
		}
		slices.SortFunc(found, func(a, b *domain.Task) int {
			if c := a.ArchivedAt.Compare(*b.ArchivedAt); c != 0 {
				return c
			}
			return strings.Compare(a.ID.String(), b.ID.String())
		})
		for _, t := range found[:min(len(found), repository.MaxArchivedIDs)] {
			ids = append(ids, t.ID)
		}
		return nil
	})
	return ids, err
}

func (r *TaskRepo) List(ctx context.Context, f repository.TaskFilter) (page repository.TaskPage, err error) {
	if f.Limit <= 0 {
		return page, fmt.Errorf("%w: limit must be positive", domain.ErrValidation)
	}
	err = r.st.do(ctx, func(s *state) error {
		var matched []*domain.Task
		for _, t := range s.tasks {
			if !matchesFilter(t, f) || f.After != nil && !before(t, *f.After) {
				continue
			}
			matched = append(matched, t)
		}
		slices.SortFunc(matched, func(a, b *domain.Task) int {
			if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
				return c
			}
			return strings.Compare(b.ID.String(), a.ID.String())
		})
		for i, t := range matched {
			if i == f.Limit {
				last := page.Tasks[f.Limit-1]
				page.Next = &repository.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}
				break
			}
			page.Tasks = append(page.Tasks, copyTask(t))
		}
		return nil
	})
	return page, err
}

// before reports whether t sorts after the cursor in descending order, that
// is (updated_at, id) < (cursor.UpdatedAt, cursor.ID).
func before(t *domain.Task, c repository.Cursor) bool {
	if cmp := t.UpdatedAt.Compare(c.UpdatedAt); cmp != 0 {
		return cmp < 0
	}
	return strings.Compare(t.ID.String(), c.ID.String()) < 0
}

func (r *TaskRepo) Delete(ctx context.Context, tenantID string, id uuid.UUID) error {
	return r.st.do(ctx, func(s *state) error {
		t, ok := s.tasks[id]
		if !ok || t.TenantID != tenantID {
			return domain.ErrNotFound
		}
		delete(s.tasks, id)
		// Comments, dependency links, and agent jobs go with the task, as with
		// ON DELETE CASCADE.
		s.comments = slices.DeleteFunc(s.comments, func(c domain.Comment) bool { return c.TaskID == id })
		s.deleteJobsForTask(id)
		// Children keep existing, as with ON DELETE SET NULL.
		for _, child := range s.tasks {
			if child.ParentID != nil && *child.ParentID == id {
				child.ParentID = nil
			}
		}
		for k := range s.deps {
			if k.task == id || k.dependsOn == id {
				delete(s.deps, k)
			}
		}
		return nil
	})
}

func (r *TaskRepo) ClaimDueSoon(ctx context.Context, now time.Time, limit int) ([]*domain.Task, error) {
	if !inTx(ctx) {
		return nil, repository.ErrTxRequired
	}
	return r.scan(&r.st.s, limit, func(t *domain.Task) bool {
		if t.DueAt == nil || !t.DueAt.After(now) || t.DueAt.After(now.Add(24*time.Hour)) {
			return false
		}
		last := t.LastDueSoonNotifiedAt
		return last == nil || (!t.DueAt.After(now.Add(time.Hour)) && last.Before(t.DueAt.Add(-time.Hour)))
	}), nil
}

func (r *TaskRepo) ClaimOverdue(ctx context.Context, now time.Time, limit int) ([]*domain.Task, error) {
	if !inTx(ctx) {
		return nil, repository.ErrTxRequired
	}
	startOfDay := now.UTC().Truncate(24 * time.Hour)
	return r.scan(&r.st.s, limit, func(t *domain.Task) bool {
		return t.DueAt != nil && !t.DueAt.After(now) &&
			(t.LastOverdueNotifiedAt == nil || t.LastOverdueNotifiedAt.Before(startOfDay))
	}), nil
}

func (r *TaskRepo) scan(s *state, limit int, match func(*domain.Task) bool) []*domain.Task {
	var out []*domain.Task
	for _, t := range s.tasks {
		if t.IsOpen() && match(t) {
			out = append(out, copyTask(t))
		}
	}
	slices.SortFunc(out, func(a, b *domain.Task) int { return a.DueAt.Compare(*b.DueAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (r *TaskRepo) ClaimStale(ctx context.Context, staleBefore, now time.Time, limit int) ([]*domain.Task, error) {
	if !inTx(ctx) {
		return nil, repository.ErrTxRequired
	}
	s := &r.st.s
	commented := map[uuid.UUID]bool{}
	for _, c := range s.comments {
		if c.CreatedAt.After(staleBefore) {
			commented[c.TaskID] = true
		}
	}
	var out []*domain.Task
	for _, t := range s.tasks {
		if t.Status != domain.StatusInProgress || t.UpdatedAt.After(staleBefore) || commented[t.ID] {
			continue
		}
		w, ok := s.workspaces[wsKey{t.TenantID, t.WorkspaceID}]
		if !ok || !w.AgentSettings.Allows(domain.AgentBlocker) {
			continue
		}
		if _, queued := s.jobKeys[domain.StaleJobDedupKey(domain.AgentBlocker, t.ID, now)]; queued {
			continue
		}
		out = append(out, copyTask(t))
	}
	slices.SortFunc(out, func(a, b *domain.Task) int {
		return cmp.Or(a.UpdatedAt.Compare(b.UpdatedAt), strings.Compare(a.ID.String(), b.ID.String()))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *TaskRepo) ListBriefRecipients(ctx context.Context, day time.Time, limit int) ([]domain.BriefRecipient, error) {
	type who struct{ tenant, user string }
	first := map[who]string{} // the first allowed workspace by ID
	err := r.st.do(ctx, func(s *state) error {
		add := func(t *domain.Task, user string) {
			w, ok := s.workspaces[wsKey{t.TenantID, t.WorkspaceID}]
			if user == "" || !ok || !w.AgentSettings.Allows(domain.AgentDigest) {
				return
			}
			if _, queued := s.jobKeys[domain.BriefJobDedupKey(t.TenantID, user, day)]; queued {
				return
			}
			k := who{t.TenantID, user}
			if cur, seen := first[k]; !seen || t.WorkspaceID < cur {
				first[k] = t.WorkspaceID
			}
		}
		for _, t := range s.tasks {
			if t.Status == domain.StatusTodo || t.Status == domain.StatusInProgress {
				add(t, t.AssigneeID)
			}
		}
		for _, j := range s.jobs {
			if j.Status != domain.JobAwaitingApproval || j.TaskID == nil {
				continue
			}
			if t := s.tasks[*j.TaskID]; t != nil {
				add(t, t.Owner())
			}
		}
		return nil
	})
	out := make([]domain.BriefRecipient, 0, len(first))
	for k, ws := range first {
		out = append(out, domain.BriefRecipient{TenantID: k.tenant, UserID: k.user, WorkspaceID: ws})
	}
	slices.SortFunc(out, func(a, b domain.BriefRecipient) int {
		return cmp.Or(strings.Compare(a.TenantID, b.TenantID), strings.Compare(a.UserID, b.UserID))
	})
	return out[:min(limit, len(out))], err
}

func (r *TaskRepo) MarkDueSoonNotified(ctx context.Context, tenantID string, id uuid.UUID, at time.Time) error {
	return r.mark(ctx, tenantID, id, func(t *domain.Task) { t.LastDueSoonNotifiedAt = microPtr(&at) })
}

func (r *TaskRepo) MarkOverdueNotified(ctx context.Context, tenantID string, id uuid.UUID, at time.Time) error {
	return r.mark(ctx, tenantID, id, func(t *domain.Task) { t.LastOverdueNotifiedAt = microPtr(&at) })
}

func (r *TaskRepo) mark(ctx context.Context, tenantID string, id uuid.UUID, set func(*domain.Task)) error {
	return r.st.do(ctx, func(s *state) error {
		t, ok := s.tasks[id]
		if !ok || t.TenantID != tenantID {
			return domain.ErrNotFound
		}
		set(t)
		return nil
	})
}

func checkBlocked(t *domain.Task) error {
	blocked := t.Status == domain.StatusBlocked
	if blocked != (t.BlockedReason != nil) || blocked != (t.StatusBeforeBlocked != nil) {
		return fmt.Errorf("%w: blocked columns do not match status", domain.ErrValidation)
	}
	return nil
}

// --- comments --------------------------------------------------------------

func (r *CommentRepo) Create(ctx context.Context, c domain.Comment) error {
	return r.st.do(ctx, func(s *state) error {
		if _, ok := s.tasks[c.TaskID]; !ok {
			return fmt.Errorf("%w: task does not exist", domain.ErrValidation)
		}
		c.CreatedAt = micro(c.CreatedAt)
		c.Mentions = slices.Clone(c.Mentions)
		if c.Mentions == nil {
			c.Mentions = []string{}
		}
		s.comments = append(s.comments, c)
		return nil
	})
}

func (r *CommentRepo) ListRecent(ctx context.Context, taskID uuid.UUID, limit int) (out []domain.Comment, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for _, c := range s.comments {
			if c.TaskID == taskID {
				c.Mentions = slices.Clone(c.Mentions)
				out = append(out, c)
			}
		}
		slices.SortFunc(out, func(a, b domain.Comment) int {
			if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
				return c
			}
			return strings.Compare(b.ID.String(), a.ID.String())
		})
		if len(out) > limit {
			out = out[:limit]
		}
		return nil
	})
	return out, err
}

// --- outbox ----------------------------------------------------------------

func (r *OutboxRepo) Insert(ctx context.Context, entries []domain.OutboxEntry) (n int, err error) {
	err = r.st.WithTx(ctx, func(ctx context.Context) error {
		s := &r.st.s
		for _, e := range entries {
			if _, dup := s.outboxKeys[e.IdempotencyKey]; dup {
				continue
			}
			e.Status, e.Attempts, e.ClaimedAt, e.DispatchedAt, e.LastError = domain.OutboxPending, 0, nil, nil, ""
			e.NextAttemptAt, e.CreatedAt = micro(e.NextAttemptAt), micro(e.CreatedAt)
			if e.TraceContext == nil {
				e.TraceContext = map[string]string{}
			}
			cp := e
			s.outbox[e.ID] = &cp
			s.outboxKeys[e.IdempotencyKey] = e.ID
			n++
		}
		return nil
	})
	return n, err
}

func (r *OutboxRepo) ClaimBatch(ctx context.Context, now time.Time, limit int) (out []domain.OutboxEntry, err error) {
	claimedAt := micro(now)
	err = r.st.do(ctx, func(s *state) error {
		var due []*domain.OutboxEntry
		for _, e := range s.outbox {
			if e.Status == domain.OutboxPending && !e.NextAttemptAt.After(claimedAt) {
				due = append(due, e)
			}
		}
		slices.SortFunc(due, func(a, b *domain.OutboxEntry) int {
			return cmp.Or(a.NextAttemptAt.Compare(b.NextAttemptAt), a.CreatedAt.Compare(b.CreatedAt))
		})
		if len(due) > limit {
			due = due[:limit]
		}
		for _, e := range due {
			e.Status = domain.OutboxProcessing
			at := claimedAt
			e.ClaimedAt = &at
			out = append(out, *e)
		}
		return nil
	})
	return out, err
}

// settle applies fn to the row if it is still held by the given claim.
func (r *OutboxRepo) settle(ctx context.Context, id uuid.UUID, claimedAt time.Time, fn func(e *domain.OutboxEntry)) error {
	return r.st.do(ctx, func(s *state) error {
		e, ok := s.outbox[id]
		if ok && e.Status == domain.OutboxProcessing && e.ClaimedAt != nil && e.ClaimedAt.Equal(claimedAt) {
			fn(e)
			e.ClaimedAt = nil
		}
		return nil
	})
}

func (r *OutboxRepo) MarkDispatched(ctx context.Context, id uuid.UUID, claimedAt, at time.Time) error {
	return r.settle(ctx, id, claimedAt, func(e *domain.OutboxEntry) {
		e.Status, e.DispatchedAt = domain.OutboxDispatched, microPtr(&at)
	})
}

func (r *OutboxRepo) MarkRetry(ctx context.Context, id uuid.UUID, claimedAt time.Time, attempts int, next time.Time, lastErr string) error {
	return r.settle(ctx, id, claimedAt, func(e *domain.OutboxEntry) {
		e.Status, e.Attempts, e.NextAttemptAt, e.LastError = domain.OutboxPending, attempts, micro(next), lastErr
	})
}

func (r *OutboxRepo) Release(ctx context.Context, id uuid.UUID, claimedAt, next time.Time) error {
	return r.settle(ctx, id, claimedAt, func(e *domain.OutboxEntry) {
		e.Status, e.NextAttemptAt = domain.OutboxPending, micro(next)
	})
}

func (r *OutboxRepo) MarkDeadLetter(ctx context.Context, id uuid.UUID, claimedAt time.Time, attempts int, lastErr string) error {
	return r.settle(ctx, id, claimedAt, func(e *domain.OutboxEntry) {
		e.Status, e.Attempts, e.LastError = domain.OutboxDeadLetter, attempts, lastErr
	})
}

func (r *OutboxRepo) Coalesce(ctx context.Context, e domain.OutboxEntry) error {
	return r.st.do(ctx, func(s *state) error {
		trace := e.TraceContext
		if trace == nil {
			trace = map[string]string{}
		}
		if id, ok := s.outboxKeys[e.IdempotencyKey]; ok {
			row := s.outbox[id]
			row.Payload, row.TraceContext, row.MaxAttempts = slices.Clone(e.Payload), trace, e.MaxAttempts
			if row.Status != domain.OutboxProcessing {
				row.Status, row.Attempts, row.NextAttemptAt, row.LastError = domain.OutboxPending, 0, micro(e.NextAttemptAt), ""
			}
			row.DispatchedAt = nil
			return nil
		}
		e.Status, e.Attempts, e.ClaimedAt, e.DispatchedAt, e.LastError = domain.OutboxPending, 0, nil, nil, ""
		e.NextAttemptAt, e.CreatedAt = micro(e.NextAttemptAt), micro(e.CreatedAt)
		e.Payload, e.TraceContext = slices.Clone(e.Payload), trace
		cp := e
		s.outbox[e.ID] = &cp
		s.outboxKeys[e.IdempotencyKey] = e.ID
		return nil
	})
}

func (r *OutboxRepo) MarkDispatchedIfUnchanged(ctx context.Context, id uuid.UUID, claimedAt time.Time, sent []byte, at time.Time) (requeued bool, err error) {
	err = r.settle(ctx, id, claimedAt, func(e *domain.OutboxEntry) {
		if bytes.Equal(e.Payload, sent) {
			e.Status, e.DispatchedAt = domain.OutboxDispatched, microPtr(&at)
			return
		}
		e.Status, e.Attempts, e.NextAttemptAt, e.LastError, requeued = domain.OutboxPending, 0, micro(at), "", true
	})
	return requeued, err
}

func (r *OutboxRepo) MarkDeadLetterIfUnchanged(ctx context.Context, id uuid.UUID, claimedAt time.Time, sent []byte, attempts int, lastErr string, at time.Time) (requeued bool, err error) {
	err = r.settle(ctx, id, claimedAt, func(e *domain.OutboxEntry) {
		if bytes.Equal(e.Payload, sent) {
			e.Status, e.Attempts, e.LastError = domain.OutboxDeadLetter, attempts, lastErr
			return
		}
		e.Status, e.Attempts, e.NextAttemptAt, e.LastError, requeued = domain.OutboxPending, 0, micro(at), "", true
	})
	return requeued, err
}

func (r *OutboxRepo) ReleaseExpiredLeases(ctx context.Context, olderThan time.Time) (n int, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for _, e := range s.outbox {
			if e.Status == domain.OutboxProcessing && e.ClaimedAt != nil && e.ClaimedAt.Before(olderThan) {
				e.Status, e.ClaimedAt = domain.OutboxPending, nil
				n++
			}
		}
		return nil
	})
	return n, err
}

func (r *OutboxRepo) DeleteDispatchedBefore(ctx context.Context, cutoff time.Time, limit int) (n int, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for id, e := range s.outbox {
			if n == limit {
				break
			}
			if e.Status == domain.OutboxDispatched && e.DispatchedAt != nil && e.DispatchedAt.Before(cutoff) {
				delete(s.outbox, id)
				delete(s.outboxKeys, e.IdempotencyKey)
				n++
			}
		}
		return nil
	})
	return n, err
}

func (r *OutboxRepo) Get(ctx context.Context, id uuid.UUID) (e domain.OutboxEntry, err error) {
	err = r.st.do(ctx, func(s *state) error {
		stored, ok := s.outbox[id]
		if !ok {
			return domain.ErrNotFound
		}
		e = *stored
		return nil
	})
	return e, err
}

func (r *OutboxRepo) DeleteForAggregate(ctx context.Context, tenantID string, aggregateID uuid.UUID) (n int, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for id, e := range s.outbox {
			if e.TenantID == tenantID && e.AggregateID == aggregateID {
				delete(s.outbox, id)
				delete(s.outboxKeys, e.IdempotencyKey)
				n++
			}
		}
		return nil
	})
	return n, err
}

func (r *OutboxRepo) ListForAggregate(ctx context.Context, aggregateID uuid.UUID) (out []domain.OutboxEntry, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for _, e := range s.outbox {
			if e.AggregateID == aggregateID {
				out = append(out, *e)
			}
		}
		slices.SortFunc(out, func(a, b domain.OutboxEntry) int {
			return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID.String(), b.ID.String()))
		})
		return nil
	})
	return out, err
}

func (r *OutboxRepo) CountByStatus(ctx context.Context, status domain.OutboxStatus) (n int, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for _, e := range s.outbox {
			if e.Status == status {
				n++
			}
		}
		return nil
	})
	return n, err
}

func (r *TaskRepo) CountAssigned(ctx context.Context, tenantID, assigneeID string, status domain.Status, priority domain.Priority) (n int, err error) {
	err = r.st.do(ctx, func(s *state) error {
		for _, t := range s.tasks {
			if t.TenantID == tenantID && t.AssigneeID == assigneeID && t.Status == status && t.Priority == priority {
				n++
			}
		}
		return nil
	})
	return n, err
}

func (r *TaskRepo) Existing(ctx context.Context, tenantID string, ids []uuid.UUID) (found []uuid.UUID, err error) {
	found = []uuid.UUID{}
	err = r.st.do(ctx, func(s *state) error {
		for _, id := range ids {
			if t, ok := s.tasks[id]; ok && t.TenantID == tenantID && !slices.Contains(found, id) {
				found = append(found, id)
			}
		}
		return nil
	})
	return found, err
}

func (r *TaskRepo) WorkspaceLabels(ctx context.Context, tenantID, workspaceID string, limit int) ([]string, error) {
	counts := map[string]int{}
	err := r.st.do(ctx, func(s *state) error {
		for _, t := range s.tasks {
			if t.TenantID == tenantID && t.WorkspaceID == workspaceID {
				for _, l := range t.Labels {
					counts[l]++
				}
			}
		}
		return nil
	})
	out := make([]string, 0, len(counts))
	for l := range counts {
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b string) int {
		if counts[a] != counts[b] {
			return counts[b] - counts[a]
		}
		return strings.Compare(a, b)
	})
	return out[:min(limit, len(out))], err
}

// SimilarOpen scores by shared search terms. PostgreSQL also stems words,
// so it can match more; the order of clear matches is the same.
func (r *TaskRepo) SimilarOpen(ctx context.Context, tenantID, workspaceID string, exclude uuid.UUID, text string, limit int) ([]*domain.Task, error) {
	want := domain.SearchTerms(text)
	type scored struct {
		t     *domain.Task
		score int
	}
	var hits []scored
	err := r.st.do(ctx, func(s *state) error {
		for _, t := range s.tasks {
			if t.TenantID != tenantID || t.WorkspaceID != workspaceID || t.ID == exclude || !t.IsOpen() {
				continue
			}
			n := 0
			for _, w := range domain.SearchTerms(t.Title) {
				if slices.Contains(want, w) {
					n++
				}
			}
			if n > 0 {
				hits = append(hits, scored{copyTask(t), n})
			}
		}
		return nil
	})
	slices.SortFunc(hits, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		if c := b.t.UpdatedAt.Compare(a.t.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.t.ID.String(), b.t.ID.String())
	})
	out := []*domain.Task{}
	for _, h := range hits[:min(limit, len(hits))] {
		out = append(out, h.t)
	}
	return out, err
}
