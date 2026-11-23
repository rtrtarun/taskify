package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/internal/repository"
)

// Field names are snake_case; timestamps are RFC 3339 in UTC (section 9.2).

type taskJSON struct {
	ID            uuid.UUID      `json:"id"`
	TenantID      string         `json:"tenant_id"`
	WorkspaceID   string         `json:"workspace_id"`
	Title         string         `json:"title"`
	Description   string         `json:"description"`
	Status        string         `json:"status"`
	Priority      string         `json:"priority"`
	CreatorID     string         `json:"creator_id"`
	AssigneeID    *string        `json:"assignee_id"`
	Labels        []string       `json:"labels"`
	Metadata      map[string]any `json:"metadata"`
	WatcherIDs    []string       `json:"watcher_ids"`
	DueAt         *time.Time     `json:"due_at"`
	RemindAt      *time.Time     `json:"remind_at"`
	CompletedAt   *time.Time     `json:"completed_at"`
	ArchivedAt    *time.Time     `json:"archived_at"`
	BlockedReason *string        `json:"blocked_reason"`
	Version       int            `json:"version"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

func toTaskJSON(t *domain.Task) taskJSON {
	j := taskJSON{
		ID: t.ID, TenantID: t.TenantID, WorkspaceID: t.WorkspaceID, Title: t.Title, Description: t.Description,
		Status: string(t.Status), Priority: string(t.Priority), CreatorID: t.CreatorID,
		Labels: t.Labels, Metadata: t.Metadata, WatcherIDs: t.Watchers,
		DueAt: t.DueAt, RemindAt: t.RemindAt, CompletedAt: t.CompletedAt, ArchivedAt: t.ArchivedAt,
		Version: t.Version, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
	if t.AssigneeID != "" {
		a := t.AssigneeID
		j.AssigneeID = &a
	}
	if t.BlockedReason != nil {
		r := string(*t.BlockedReason)
		j.BlockedReason = &r
	}
	if j.Labels == nil {
		j.Labels = []string{}
	}
	if j.WatcherIDs == nil {
		j.WatcherIDs = []string{}
	}
	if j.Metadata == nil {
		j.Metadata = map[string]any{}
	}
	return j
}

type commentJSON struct {
	ID        uuid.UUID `json:"id"`
	TaskID    uuid.UUID `json:"task_id"`
	AuthorID  string    `json:"author_id"`
	Body      string    `json:"body"`
	Mentions  []string  `json:"mentions"`
	CreatedAt time.Time `json:"created_at"`
}

func toCommentJSON(c domain.Comment) commentJSON {
	m := c.Mentions
	if m == nil {
		m = []string{}
	}
	return commentJSON{ID: c.ID, TaskID: c.TaskID, AuthorID: c.AuthorID, Body: c.Body, Mentions: m, CreatedAt: c.CreatedAt}
}

type taskDetailJSON struct {
	taskJSON
	Comments []commentJSON `json:"comments"`
}

type taskListJSON struct {
	Tasks      []taskJSON `json:"tasks"`
	NextCursor *string    `json:"next_cursor"`
	// ArchivedIDs is sent only with updated_since.
	ArchivedIDs []uuid.UUID `json:"archived_ids,omitzero"`
}

type createTaskRequest struct {
	WorkspaceID string         `json:"workspace_id"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Priority    string         `json:"priority"`
	AssigneeID  string         `json:"assignee_id"`
	Labels      []string       `json:"labels"`
	Metadata    map[string]any `json:"metadata"`
	WatcherIDs  []string       `json:"watcher_ids"`
	DueAt       *time.Time     `json:"due_at"`
	RemindAt    *time.Time     `json:"remind_at"`
}

// optional tells an absent field from an explicit null in a PATCH body.
type optional[T any] struct {
	Set   bool
	Null  bool
	Value T
}

func (o *optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.Value)
}

type updateTaskRequest struct {
	Version     optional[int]            `json:"version"`
	Title       optional[string]         `json:"title"`
	Description optional[string]         `json:"description"`
	Status      optional[string]         `json:"status"`
	Priority    optional[string]         `json:"priority"`
	AssigneeID  optional[string]         `json:"assignee_id"`
	Labels      optional[[]string]       `json:"labels"`
	Metadata    optional[map[string]any] `json:"metadata"`
	WatcherIDs  optional[[]string]       `json:"watcher_ids"`
	DueAt       optional[time.Time]      `json:"due_at"`
	RemindAt    optional[time.Time]      `json:"remind_at"`
}

// patch converts the request to a domain patch. null clears a nullable field
// and empties a list; it is not allowed for a required field.
func (u updateTaskRequest) patch() (domain.Patch, error) {
	var p domain.Patch
	required := func(name string, o optional[string]) (*string, error) {
		if !o.Set {
			return nil, nil
		}
		if o.Null {
			return nil, fmt.Errorf("%w: %s cannot be null", domain.ErrValidation, name)
		}
		v := o.Value
		return &v, nil
	}
	var err error
	if p.Title, err = required("title", u.Title); err != nil {
		return p, err
	}
	if p.Description, err = required("description", u.Description); err != nil {
		return p, err
	}
	status, err := required("status", u.Status)
	if err != nil {
		return p, err
	}
	if status != nil {
		s := domain.Status(*status)
		p.Status = &s
	}
	prio, err := required("priority", u.Priority)
	if err != nil {
		return p, err
	}
	if prio != nil {
		pr := domain.Priority(*prio)
		p.Priority = &pr
	}
	if u.AssigneeID.Set {
		a := u.AssigneeID.Value // null unassigns
		p.AssigneeID = &a
	}
	if u.Labels.Set {
		l := u.Labels.Value
		p.Labels = &l
	}
	if u.Metadata.Set {
		m := u.Metadata.Value
		p.Metadata = &m
	}
	if u.WatcherIDs.Set {
		w := u.WatcherIDs.Value
		p.Watchers = &w
	}
	p.DueAt = timePatch(u.DueAt)
	p.RemindAt = timePatch(u.RemindAt)
	return p, nil
}

func timePatch(o optional[time.Time]) domain.TimePatch {
	switch {
	case !o.Set:
		return domain.TimePatch{}
	case o.Null:
		return domain.ClearTime()
	default:
		return domain.SetTime(o.Value)
	}
}

// Cursors are opaque to clients: base64url of "<unix microseconds>:<uuid>".

func encodeCursor(c *repository.Cursor) *string {
	if c == nil {
		return nil
	}
	s := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%s", c.UpdatedAt.UnixMicro(), c.ID)))
	return &s
}

func decodeCursor(s string) (*repository.Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, badRequest("cursor is not valid")
	}
	micros, id, ok := strings.Cut(string(raw), ":")
	if !ok {
		return nil, badRequest("cursor is not valid")
	}
	n, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return nil, badRequest("cursor is not valid")
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return nil, badRequest("cursor is not valid")
	}
	return &repository.Cursor{UpdatedAt: time.UnixMicro(n).UTC(), ID: u}, nil
}

// etag is the strong validator for a task: its version.
func etag(version int) string { return `"` + strconv.Itoa(version) + `"` }

// detailETag is the validator of a task detail: the version, a dot, and a
// hash of the body, such as "3.9f2c4a1b7d03". The version leads so that a
// client that sends this tag back as If-Match still names its version.
func detailETag(version int, body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + strconv.Itoa(version) + "." + hex.EncodeToString(sum[:6]) + `"`
}

// parseIfMatch accepts "3", W/"3", 3, and the tag of a task detail, "3.9f2c4a1b7d03".
func parseIfMatch(h string) (int, error) {
	v := strings.TrimPrefix(strings.TrimSpace(h), "W/")
	v = strings.Trim(v, `"`)
	v, _, _ = strings.Cut(v, ".")
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, badRequest("If-Match must hold the task version, such as \"3\"")
	}
	return n, nil
}
