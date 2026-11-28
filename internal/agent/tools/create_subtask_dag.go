package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/rtrtarun/taskify/internal/domain"
)

// SubtaskCreator is the part of service.TaskService CreateSubtaskDAG uses.
type SubtaskCreator interface {
	CreateSubtasks(ctx context.Context, actor domain.Actor, tenantID string, parentID uuid.UUID,
		expectedVersion int, creatorID string, specs []domain.SubtaskSpec, run domain.AgentRunRecord) (domain.SubtaskResult, error)
}

// CreateSubtaskDAGName is the tool's name (section 5.7).
const CreateSubtaskDAGName = "CreateSubtaskDAG"

// CreateSubtaskDAG splits the job's task into subtasks with dependencies
// between them (section 3.8). It is Tier 1, since a person asked for the
// decomposition, unless the workspace requires approval for everything.
//
// Check refuses, before anything runs or is proposed, a plan that names a
// parent other than the job's task, exceeds the workspace's cap, repeats a
// title, depends on a title that is not a sibling, or has a cycle. The model
// is told why and gets one repair turn. Execute creates everything in one
// transaction through TaskService.CreateSubtasks, which checks the plan
// again against the current settings.
type CreateSubtaskDAG struct {
	Tasks SubtaskCreator
}

// CreateSubtaskDAGArgs are the tool's arguments.
type CreateSubtaskDAGArgs struct {
	ParentTaskID uuid.UUID            `json:"parent_task_id"`
	Subtasks     []domain.SubtaskSpec `json:"subtasks"`
	Reasoning    string               `json:"reasoning,omitempty"`
}

// SubtaskJSON describes one created subtask to the model.
type SubtaskJSON struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// SubtaskDAGResult is the tool's output. The parent's version does not
// change, so it is not a VersionedOutput.
type SubtaskDAGResult struct {
	Created  bool          `json:"created"`
	Subtasks []SubtaskJSON `json:"subtasks"`
}

func (CreateSubtaskDAG) Name() string { return CreateSubtaskDAGName }

func (CreateSubtaskDAG) Description() string {
	return "Split the task into subtasks, each with a title, an optional description, and depends_on: " +
		"the exact titles of other subtasks in this same call that must be finished first. " +
		"All subtasks are created together, or none are."
}

func (CreateSubtaskDAG) Tier(json.RawMessage) domain.SafetyTier { return domain.Tier1 }

func parseDAG(args json.RawMessage) (CreateSubtaskDAGArgs, error) {
	var a CreateSubtaskDAGArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, fmt.Errorf("%w: %s", domain.ErrValidation, err.Error())
	}
	return a, nil
}

// Check applies the rules the schema cannot: the parent is the job's own
// task, and the subtasks form a valid plan within the workspace's cap.
func (CreateSubtaskDAG) Check(s Scope, args json.RawMessage) error {
	a, err := parseDAG(args)
	if err != nil {
		return err
	}
	if s.TaskID != nil && a.ParentTaskID != *s.TaskID {
		return fmt.Errorf("parent_task_id must be the task this job decomposes, %s", *s.TaskID)
	}
	if _, err := domain.PlanSubtasks(a.Subtasks, s.MaxSubtasks); err != nil {
		if s.MaxSubtasks > 0 && len(a.Subtasks) > s.MaxSubtasks {
			return fmt.Errorf("%w. Merge or drop subtasks and call %s again with at most %d",
				err, CreateSubtaskDAGName, s.MaxSubtasks)
		}
		return err
	}
	return nil
}

// Execute creates the subtasks as the agent, expecting the parent at the
// scope's base version. Their creator is the person who asked for the
// decomposition.
func (c CreateSubtaskDAG) Execute(ctx context.Context, s Scope, args json.RawMessage) (any, error) {
	a, err := parseDAG(args)
	if err != nil {
		return nil, err
	}
	if s.RequestedBy == "" {
		return nil, fmt.Errorf("%w: this job was not requested by a person, so its subtasks would have no creator", domain.ErrValidation)
	}
	res, err := c.Tasks.CreateSubtasks(ctx, s.Actor(), s.TenantID, a.ParentTaskID, s.BaseVersion, s.RequestedBy, a.Subtasks,
		domain.AgentRunRecord{JobID: s.JobID, Agent: s.AgentType, Reasoning: a.Reasoning})
	if err != nil {
		return nil, err
	}
	out := SubtaskDAGResult{Created: res.Created, Subtasks: make([]SubtaskJSON, len(res.Subtasks))}
	for i, t := range res.Subtasks {
		out.Subtasks[i] = SubtaskJSON{ID: t.ID.String(), Title: t.Title, Status: string(t.Status)}
	}
	return out, nil
}
