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

	"github.com/google/uuid"

	"github.com/RONGALI-TARUN/taskify/internal/domain"
	"github.com/RONGALI-TARUN/taskify/internal/repository"
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
}

// New returns an empty store.
func New() *Store {
	return &Store{s: state{
		workspaces: map[wsKey]domain.Workspace{},
		tasks:      map[uuid.UUID]*domain.Task{},
		outbox:     map[uuid.UUID]*domain.OutboxEntry{},
		outboxKeys: map[string]uuid.UUID{},
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
