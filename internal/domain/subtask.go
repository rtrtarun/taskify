package domain

import (
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// maxSubtaskDescriptionLen bounds a subtask's description as written by a
// model; a person can edit it afterwards like any other task.
const maxSubtaskDescriptionLen = 4000

// SubtaskSpec is one subtask a decomposition asks for. DependsOn names other
// subtasks of the same decomposition by title.
type SubtaskSpec struct {
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

// PlannedSubtask is a subtask with its dependencies resolved.
type PlannedSubtask struct {
	SubtaskSpec
	// Blockers are indexes into the plan of the siblings this subtask waits
	// for. Each is smaller than the subtask's own index.
	Blockers []int
}

// PlanSubtasks checks a decomposition and returns its subtasks in an order
// where every subtask comes after the siblings it depends on (section 3.8).
// Titles are compared trimmed and case-insensitively, so they must be unique
// in that sense. It rejects, with ErrValidation:
//
//   - no subtasks, or more than max when max is positive;
//   - an empty, too long, or repeated title;
//   - a depends_on title that names no sibling, or the subtask itself;
//   - dependencies that form a cycle.
//
// Among subtasks free to go next, the order given is kept, so the plan is
// deterministic.
func PlanSubtasks(specs []SubtaskSpec, max int) ([]PlannedSubtask, error) {
	switch {
	case len(specs) == 0:
		return nil, invalid("a decomposition needs at least one subtask")
	case max > 0 && len(specs) > max:
		return nil, invalid("%d subtasks proposed; this workspace allows at most %d per decomposition", len(specs), max)
	}
	index := make(map[string]int, len(specs))
	for i, s := range specs {
		title := strings.TrimSpace(s.Title)
		switch {
		case title == "":
			return nil, invalid("subtask %d has no title", i+1)
		case utf8.RuneCountInString(title) > maxTitleLen:
			return nil, invalid("subtask %q: a title must be at most %d characters", title, maxTitleLen)
		case utf8.RuneCountInString(s.Description) > maxSubtaskDescriptionLen:
			return nil, invalid("subtask %q: a description must be at most %d characters", title, maxSubtaskDescriptionLen)
		}
		key := strings.ToLower(title)
		if _, dup := index[key]; dup {
			return nil, invalid("two subtasks are titled %q; titles must be unique", title)
		}
		index[key] = i
	}

	// deps[i] lists the indexes, in the given order, that subtask i waits for.
	deps := make([][]int, len(specs))
	for i, s := range specs {
		for _, name := range s.DependsOn {
			j, ok := index[strings.ToLower(strings.TrimSpace(name))]
			switch {
			case !ok:
				return nil, invalid("subtask %q depends on %q, which is not one of the subtasks", strings.TrimSpace(s.Title), name)
			case j == i:
				return nil, invalid("subtask %q cannot depend on itself", strings.TrimSpace(s.Title))
			}
			if !slices.Contains(deps[i], j) {
				deps[i] = append(deps[i], j)
			}
		}
	}

	// Kahn's algorithm, always taking the earliest ready subtask.
	waiting := make([]int, len(specs))
	dependents := make([][]int, len(specs))
	for i, ds := range deps {
		waiting[i] = len(ds)
		for _, j := range ds {
			dependents[j] = append(dependents[j], i)
		}
	}
	placed := make([]int, len(specs)) // original index -> plan index, or -1
	for i := range placed {
		placed[i] = -1
	}
	plan := make([]PlannedSubtask, 0, len(specs))
	for len(plan) < len(specs) {
		next := -1
		for i := range specs {
			if placed[i] < 0 && waiting[i] == 0 {
				next = i
				break
			}
		}
		if next < 0 {
			var cyclic []string
			for i, s := range specs {
				if placed[i] < 0 {
					cyclic = append(cyclic, strings.TrimSpace(s.Title))
				}
			}
			return nil, invalid("the dependencies between these subtasks form a cycle: %s", strings.Join(cyclic, ", "))
		}
		placed[next] = len(plan)
		p := PlannedSubtask{SubtaskSpec: specs[next]}
		p.Title = strings.TrimSpace(p.Title)
		p.DependsOn = slices.Clone(p.DependsOn)
		for _, j := range deps[next] {
			p.Blockers = append(p.Blockers, placed[j])
		}
		plan = append(plan, p)
		for _, d := range dependents[next] {
			waiting[d]--
		}
	}
	return plan, nil
}

// NewSubtask returns a child of parent for a planned subtask, at version 1.
// It takes the parent's workspace, priority, and assignee; creatorID is the
// person who asked for the decomposition. A subtask with blockers starts
// blocked with reason dependency, as if it had been todo, and raises no
// event: it is quiet until its blockers resolve and task.unblocked is sent.
// One without blockers raises task.created, by actor (the agent), so the
// assignee hears of it whoever the creator is.
func NewSubtask(parent *Task, p PlannedSubtask, creatorID string, actor Actor, now time.Time) (*Task, []Event, error) {
	t, _, err := NewTask(NewTaskInput{
		TenantID:    parent.TenantID,
		WorkspaceID: parent.WorkspaceID,
		Title:       p.Title,
		Description: p.Description,
		Priority:    parent.Priority,
		AssigneeID:  parent.AssigneeID,
	}, User(creatorID), now)
	if err != nil {
		return nil, nil, err
	}
	pid := parent.ID
	t.ParentID = &pid
	if len(p.Blockers) > 0 {
		before, reason := StatusTodo, BlockedDependency
		t.Status = StatusBlocked
		t.StatusBeforeBlocked = &before
		t.BlockedReason = &reason
		return t, nil, nil
	}
	return t, []Event{t.event(EventTaskCreated, actor, now.UTC())}, nil
}

// SubtaskResult is what a decomposition made.
type SubtaskResult struct {
	// Subtasks are the children in plan order: every subtask comes after
	// the siblings it depends on.
	Subtasks []*Task
	// Created is false when the job had already created them, and they
	// were returned as they are now.
	Created bool
}

// SubtaskIDs returns the IDs of the subtasks a decomposer run recorded on
// its parent, or nil when the record lists none.
func (r AgentRunRecord) SubtaskIDs() []uuid.UUID {
	var raw []string
	switch v := r.Changes["subtask_ids"].(type) {
	case []string:
		raw = v
	case []any: // after a round trip through JSON
		for _, x := range v {
			if s, ok := x.(string); ok {
				raw = append(raw, s)
			}
		}
	}
	var out []uuid.UUID
	for _, s := range raw {
		if id, err := uuid.Parse(s); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// RunOf returns the last record jobID left on the task, if any.
func (t *Task) RunOf(jobID uuid.UUID) (AgentRunRecord, bool) {
	for i := len(t.AgentMetadata.Runs) - 1; i >= 0; i-- {
		if r := t.AgentMetadata.Runs[i]; r.JobID == jobID {
			return r, true
		}
	}
	return AgentRunRecord{}, false
}
