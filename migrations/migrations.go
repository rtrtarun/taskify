// Package migrations embeds the SQL migrations so tests (and, later, the
// server) can apply them without the golang-migrate CLI.
//
// Production deployments apply them with golang-migrate (make migrate-up),
// which records progress in schema_migrations. Apply here does not: it is for
// throwaway test databases.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

//go:embed *.sql
var FS embed.FS

// Direction selects up or down scripts.
type Direction string

const (
	Up   Direction = "up"
	Down Direction = "down"
)

// Files returns the migration file names for a direction, in the order they
// must run: ascending for up, descending for down.
func Files(d Direction) ([]string, error) {
	entries, err := fs.Glob(FS, "*."+string(d)+".sql")
	if err != nil {
		return nil, err
	}
	slices.Sort(entries)
	if d == Down {
		slices.Reverse(entries)
	}
	return entries, nil
}

// Apply runs every script for the direction against exec, in order.
func Apply(ctx context.Context, exec func(ctx context.Context, sql string) error, d Direction) error {
	files, err := Files(d)
	if err != nil {
		return err
	}
	for _, name := range files {
		body, err := FS.ReadFile(name)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(body)) == "" {
			continue
		}
		if err := exec(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}
