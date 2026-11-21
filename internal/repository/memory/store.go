// Package memory implements the repository interfaces in memory, for fast
// unit tests. It follows the same contract as the postgres package; the
// conformance suite in repotest runs against both.
//
// A transaction holds the store's single lock for its whole duration, so
// transactions are serialised. That is stricter than PostgreSQL, and makes
// row locks and SKIP LOCKED trivially correct. A failed transaction restores
// a snapshot taken when it began.
package memory

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// Store holds all data. Create one with New and pass it to each repository.
type Store struct {
	mu sync.Mutex
	s  state
}

type wsKey struct{ tenant, id string }

type state struct {
	workspaces map[wsKey]domain.Workspace
	tasks      map[uuid.UUID]*domain.Task
	comments   []domain.Comment
	outbox     map[uuid.UUID]*domain.OutboxEntry
	outboxKeys map[string]uuid.UUID
	rules      map[ruleKey]domain.WorkspaceRule
	deliveries map[string]time.Time
	deps       map[depKey]domain.Dependency
	jobs       map[uuid.UUID]*domain.AgentJob
	jobKeys    map[string]uuid.UUID
	runs       []domain.AgentRun
}

type ruleKey struct{ tenant, workspace, key string }

type depKey struct{ task, dependsOn uuid.UUID }

// New returns an empty store.
func New() *Store {
	return &Store{s: state{
		workspaces: map[wsKey]domain.Workspace{},
		tasks:      map[uuid.UUID]*domain.Task{},
		outbox:     map[uuid.UUID]*domain.OutboxEntry{},
		outboxKeys: map[string]uuid.UUID{},
		rules:      map[ruleKey]domain.WorkspaceRule{},
		deliveries: map[string]time.Time{},
		deps:       map[depKey]domain.Dependency{},
		jobs:       map[uuid.UUID]*domain.AgentJob{},
		jobKeys:    map[string]uuid.UUID{},
	}}
}

type txKey struct{}

var _ repository.TxManager = (*Store)(nil)

// WithTx implements repository.TxManager.
func (st *Store) WithTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if inTx(ctx) {
		return fn(ctx)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	snapshot := st.s.clone()
	defer func() {
		if p := recover(); p != nil {
			st.s = snapshot
			panic(p)
		}
		if err != nil {
			st.s = snapshot
		}
	}()
	return fn(context.WithValue(ctx, txKey{}, true))
}

func inTx(ctx context.Context) bool { return ctx.Value(txKey{}) != nil }

// do runs fn with the lock held, unless ctx already holds it through a
// transaction.
func (st *Store) do(ctx context.Context, fn func(s *state) error) error {
	if inTx(ctx) {
		return fn(&st.s)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return fn(&st.s)
}

func (s state) clone() state {
	c := state{
		workspaces: maps.Clone(s.workspaces),
		tasks:      make(map[uuid.UUID]*domain.Task, len(s.tasks)),
		comments:   slices.Clone(s.comments),
		outbox:     make(map[uuid.UUID]*domain.OutboxEntry, len(s.outbox)),
		outboxKeys: maps.Clone(s.outboxKeys),
		rules:      maps.Clone(s.rules),
		deliveries: maps.Clone(s.deliveries),
		deps:       maps.Clone(s.deps),
		jobs:       make(map[uuid.UUID]*domain.AgentJob, len(s.jobs)),
		jobKeys:    maps.Clone(s.jobKeys),
		runs:       slices.Clone(s.runs),
	}
	for id, j := range s.jobs {
		c.jobs[id] = copyJob(j)
	}
	for id, t := range s.tasks {
		c.tasks[id] = copyTask(t)
	}
	for id, e := range s.outbox {
		cp := *e
		c.outbox[id] = &cp
	}
	return c
}

// copyTask returns a copy sharing no slices or maps. Time pointers are
// shared: nothing mutates a time through its pointer.
func copyTask(t *domain.Task) *domain.Task {
	c := *t
	c.Labels = slices.Clone(t.Labels)
	c.Watchers = slices.Clone(t.Watchers)
	c.Metadata = maps.Clone(t.Metadata)
	if t.ParentID != nil {
		p := *t.ParentID
		c.ParentID = &p
	}
	if c.Labels == nil {
		c.Labels = []string{}
	}
	if c.Watchers == nil {
		c.Watchers = []string{}
	}
	if c.Metadata == nil {
		c.Metadata = map[string]any{}
	}
	return &c
}
