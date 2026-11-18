package domain

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const maxWorkspaceNameLen = 255

var workspaceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Workspace groups tasks within a tenant. A task can only be created in an
// existing workspace.
type Workspace struct {
	TenantID  string
	ID        string
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewWorkspace validates and returns a workspace.
func NewWorkspace(tenantID, id, name string, now time.Time) (Workspace, error) {
	w := Workspace{
		TenantID:  strings.TrimSpace(tenantID),
		ID:        strings.TrimSpace(id),
		Name:      strings.TrimSpace(name),
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}
	switch {
	case w.TenantID == "":
		return Workspace{}, invalid("tenant_id is required")
	case len(w.TenantID) > maxTenantLen:
		return Workspace{}, invalid("tenant_id must be at most %d characters", maxTenantLen)
	case !workspaceIDRe.MatchString(w.ID):
		return Workspace{}, invalid("workspace id must be 1 to 64 letters, digits, underscores, or hyphens")
	case w.Name == "":
		return Workspace{}, invalid("workspace name is required")
	case utf8.RuneCountInString(w.Name) > maxWorkspaceNameLen:
		return Workspace{}, invalid("workspace name must be at most %d characters", maxWorkspaceNameLen)
	}
	return w, nil
}
