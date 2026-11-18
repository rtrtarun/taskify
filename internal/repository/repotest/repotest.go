// Package repotest provides test backends for the repository interfaces:
// the in-memory store, and PostgreSQL when DATABASE_URL is set.
//
// Each PostgreSQL backend gets its own freshly migrated database, created
// from DATABASE_URL and dropped when the test ends, so packages and tests can
// run in parallel without sharing rows. The connecting role needs CREATEDB.
package repotest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	neturl "net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rtrtarun/taskify/internal/repository"
	"github.com/rtrtarun/taskify/internal/repository/memory"
	"github.com/rtrtarun/taskify/internal/repository/postgres"
	"github.com/rtrtarun/taskify/migrations"
)

// Backend is one complete set of repositories sharing a store.
type Backend struct {
	Name       string
	Tx         repository.TxManager
	Tasks      repository.TaskRepository
	Workspaces repository.WorkspaceRepository
	Comments   repository.CommentRepository
	Outbox     repository.OutboxRepository
	Rules      repository.WorkspaceRuleRepository
	Deliveries repository.WebhookDeliveryRepository
	Deps       repository.DependencyRepository
	// DB is set for the PostgreSQL backend only.
	DB *postgres.DB
}

// Factory creates a fresh, empty backend for one test.
type Factory struct {
	Name string
	New  func(t *testing.T) Backend
}

// Factories returns the memory factory, plus PostgreSQL when DATABASE_URL is
// set. Run a test once per factory with t.Run(f.Name, ...).
func Factories(t *testing.T) []Factory {
	t.Helper()
	fs := []Factory{{Name: "memory", New: func(*testing.T) Backend { return Memory() }}}
	if os.Getenv("DATABASE_URL") != "" {
		fs = append(fs, Factory{Name: "postgres", New: func(t *testing.T) Backend { return Postgres(t) }})
	} else {
		t.Log("DATABASE_URL is not set: PostgreSQL tests are skipped")
	}
	return fs
}

// Memory returns a backend on a new in-memory store.
func Memory() Backend {
	st := memory.New()
	return Backend{
		Name: "memory", Tx: st,
		Tasks: st.Tasks(), Workspaces: st.Workspaces(), Comments: st.Comments(), Outbox: st.Outbox(),
		Rules: st.Rules(), Deliveries: st.Deliveries(), Deps: st.Dependencies(),
	}
}

// Postgres returns a backend on a new, migrated database. It skips the test
// when DATABASE_URL is not set.
func Postgres(t testing.TB) Backend {
	t.Helper()
	db := NewDatabase(t)
	return Backend{
		Name: "postgres", Tx: db, DB: db,
		Tasks: postgres.NewTaskRepo(db), Workspaces: postgres.NewWorkspaceRepo(db),
		Comments: postgres.NewCommentRepo(db), Outbox: postgres.NewOutboxRepo(db),
		Rules: postgres.NewRuleRepo(db), Deliveries: postgres.NewDeliveryRepo(db),
		Deps: postgres.NewDependencyRepo(db),
	}
}

// NewDatabase creates and migrates a database for one test and returns a
// pool on it. The database is dropped when the test ends.
func NewDatabase(t testing.TB) *postgres.DB {
	t.Helper()
	_, db := NewDatabaseURL(t)
	return db
}

// NewDatabaseURL is NewDatabase, also returning the database's connection
// URL, for tests that start the server as a separate process.
func NewDatabaseURL(t testing.TB) (string, *postgres.DB) {
	t.Helper()
	url, pool := newEmpty(t)
	err := migrations.Apply(context.Background(), func(ctx context.Context, sql string) error {
		_, err := pool.Exec(ctx, sql)
		return err
	}, migrations.Up)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return url, &postgres.DB{Pool: pool}
}

// NewEmptyDatabase creates a database with no schema and returns a pool on
// it. The database is dropped when the test ends.
func NewEmptyDatabase(t testing.TB) *pgxpool.Pool {
	t.Helper()
	_, pool := newEmpty(t)
	return pool
}

func newEmpty(t testing.TB) (string, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect to DATABASE_URL: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	name := "taskify_test_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 20
	dbURL := withDatabase(url, name)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Logf("drop %s: %v", name, err)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	return dbURL, pool
}

// withDatabase returns url with its database name replaced.
func withDatabase(raw, name string) string {
	u, err := neturl.Parse(raw)
	if err != nil {
		return raw
	}
	u.Path = "/" + name
	return u.String()
}

func randomSuffix(t testing.TB) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d_%s", os.Getpid(), hex.EncodeToString(b))
}
