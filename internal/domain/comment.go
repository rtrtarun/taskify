package domain

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const maxCommentLen = 10000

// Comment is a note on a task.
type Comment struct {
	ID        uuid.UUID
	TaskID    uuid.UUID
	AuthorID  string
	Body      string
	Mentions  []string
	CreatedAt time.Time
}

// NewComment validates a comment on the task and returns it with the
// task.comment_added event. Adding a comment does not change the task's
// version.
func NewComment(t *Task, body string, actor Actor, now time.Time) (Comment, []Event, error) {
	body = strings.TrimSpace(body)
	switch {
	case body == "":
		return Comment{}, nil, invalid("comment body is required")
	case len(body) > maxCommentLen:
		return Comment{}, nil, invalid("comment body must be at most %d characters", maxCommentLen)
	case strings.TrimSpace(actor.ID) == "":
		return Comment{}, nil, invalid("comment author is required")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Comment{}, nil, err
	}
	now = now.UTC()
	c := Comment{
		ID:        id,
		TaskID:    t.ID,
		AuthorID:  actor.ID,
		Body:      body,
		Mentions:  ExtractMentions(body),
		CreatedAt: now,
	}
	e := t.event(EventTaskCommentAdded, actor, now)
	e.Comment = &c
	return c, []Event{e}, nil
}

// A mention is "@" followed by a username, where the "@" starts the text or
// follows a character that cannot be part of an email local part. That keeps
// "dev@example.com" from mentioning "example.com".
var mentionRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.@+-])@([A-Za-z0-9_][A-Za-z0-9_.-]*)`)

// ExtractMentions returns the distinct usernames mentioned in body, in order
// of first appearance. It never returns nil.
func ExtractMentions(body string) []string {
	out := []string{}
	for _, m := range mentionRe.FindAllStringSubmatch(body, -1) {
		// Trailing punctuation belongs to the sentence, not the name.
		name := strings.TrimRight(m[1], ".-")
		if name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}
