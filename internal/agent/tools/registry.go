// Package tools holds the tool registry: the only path from a model's answer
// to a change in Taskify (docs/architecture.md sections 5.3 and 5.7).
//
// The model can only ask for a registered tool by name. Typed Go code then
// decides whether and how the request runs: Dispatch rejects unknown tools,
// tools not offered to the job's agent, arguments that fail the tool's JSON
// Schema, and task IDs outside the job's scope, and it turns every call into
// a proposal when the workspace requires approval for everything.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/rtrtarun/taskify/internal/agent/llm"
	"github.com/rtrtarun/taskify/internal/domain"
)

// Scope is what a tool call may touch. It is built from the agent_jobs row
// and the workspace settings, never from model output.
type Scope struct {
	TenantID string
	// TaskID is the job's task, or nil for a job scoped to a user or tenant.
	TaskID *uuid.UUID
	JobID  uuid.UUID
	// BaseVersion is the task version the job started from. Tier 1 tools
	// pass it to TaskService as the expected version, so a person's edit
	// made meanwhile wins with ErrVersionConflict.
	BaseVersion int
	AgentType   string
	// AllowedTools are the tools offered to this agent. Nil allows every
	// registered tool.
	AllowedTools []string
	// RequireHITL makes every call a Tier 2 proposal (require_hitl_for_all).
	RequireHITL bool
}

// NewScope builds the scope of a job from its row and its workspace's
// settings.
func NewScope(job *domain.AgentJob, settings domain.AgentSettings, allowed []string) Scope {
	s := Scope{
		TenantID:     job.TenantID,
		JobID:        job.ID,
		AgentType:    job.AgentType,
		AllowedTools: slices.Clone(allowed),
		RequireHITL:  settings.RequireHITLForAll,
	}
	if job.TaskID != nil {
		id := *job.TaskID
		s.TaskID = &id
	}
	if job.BaseTaskVersion != nil {
		s.BaseVersion = *job.BaseTaskVersion
	}
	return s
}

// Actor is who a Tier 1 change is recorded as: the agent, so the change
// notifies people but never queues more agent work.
func (s Scope) Actor() domain.Actor { return domain.Agent(s.AgentType) }

// Tool is one action an agent may request.
type Tool interface {
	Name() string
	Description() string
	// Parameters is the JSON Schema the arguments must satisfy.
	Parameters() json.RawMessage
	// Tier classifies a call with validated arguments.
	Tier(args json.RawMessage) domain.SafetyTier
	// Execute runs a Tier 1 call with validated, in-scope arguments. Each
	// call is one transaction, so a failure leaves no partial change.
	Execute(ctx context.Context, scope Scope, args json.RawMessage) (any, error)
}

// TaskLookup loads a task within a tenant. repository.TaskRepository
// satisfies it.
type TaskLookup interface {
	Get(ctx context.Context, tenantID string, id uuid.UUID) (*domain.Task, error)
}

// Rejection codes. A rejection goes back to the model as the tool's result.
const (
	CodeUnknownTool      = "unknown_tool"
	CodeNotAllowed       = "tool_not_allowed"
	CodeInvalidArguments = "invalid_arguments"
	CodeOutOfScope       = "out_of_scope"
)

// ErrRejected matches every CallError with errors.Is.
var ErrRejected = errors.New("tool call rejected")

// CallError is a tool call refused before anything ran. The message is
// written for the model, so the runtime can send it back for repair; it
// never says whether a task outside the scope exists.
type CallError struct {
	Tool    string
	Code    string
	Message string
}

func (e *CallError) Error() string {
	return fmt.Sprintf("tool %q rejected (%s): %s", e.Tool, e.Code, e.Message)
}

func (e *CallError) Is(target error) bool { return target == ErrRejected }

// Proposal is a Tier 2 call stored for a person to approve.
type Proposal struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	CallID    string          `json:"call_id,omitempty"`
}

// Result is the outcome of an accepted call.
type Result struct {
	Tier domain.SafetyTier
	// Output is the tool's result, for a Tier 1 call that ran.
	Output any
	// Proposal is set instead for a Tier 2 call, which did not run.
	Proposal *Proposal
}

// Executed reports whether the call ran.
func (r Result) Executed() bool { return r.Proposal == nil }

// Argument names holding task IDs, checked against the scope. A target is a
// task the call changes: it must be the job's task or one of its children.
// A reference only names another task, such as a dependency: it must be in
// the job's tenant.
var (
	targetKeys    = []string{"task_id", "parent_task_id"}
	referenceKeys = []string{"depends_on_task_ids"}
)

// Registry holds the registered tools.
type Registry struct {
	lookup TaskLookup
	mu     sync.RWMutex
	tools  map[string]entry
}

type entry struct {
	tool   Tool
	schema *jsonschema.Schema
}

// NewRegistry returns an empty registry that resolves task IDs with lookup.
func NewRegistry(lookup TaskLookup) *Registry {
	return &Registry{lookup: lookup, tools: map[string]entry{}}
}

// Register adds a tool, compiling its schema. A duplicate name or a schema
// that does not compile is an error.
func (r *Registry) Register(t Tool) error {
	name := t.Name()
	if name == "" {
		return errors.New("tools: a tool needs a name")
	}
	schema, err := compile(name, t.Parameters())
	if err != nil {
		return fmt.Errorf("tools: %s: %w", name, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("tools: %s is already registered", name)
	}
	r.tools[name] = entry{tool: t, schema: schema}
	return nil
}

func compile(name string, params json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(params))
	if err != nil {
		return nil, fmt.Errorf("parameters: %w", err)
	}
	if obj, ok := doc.(map[string]any); !ok || obj["type"] != "object" {
		return nil, errors.New(`parameters must be a schema with "type": "object"`)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat() // so "format": "uuid" is enforced, not only annotated
	url := "tool:///" + name + ".json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

// Schemas returns the named tools in the form offered to the model.
func (r *Registry) Schemas(names []string) ([]llm.ToolSchema, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]llm.ToolSchema, 0, len(names))
	for _, n := range names {
		e, ok := r.tools[n]
		if !ok {
			return nil, fmt.Errorf("tools: %s is not registered", n)
		}
		out = append(out, llm.ToolSchema{Name: n, Description: e.tool.Description(), Parameters: e.tool.Parameters()})
	}
	return out, nil
}

// Dispatch checks a model's tool call and runs it if it is Tier 1. A refused
// call returns a *CallError and runs nothing. Errors from the tool itself,
// such as domain.ErrVersionConflict, are returned as they are.
func (r *Registry) Dispatch(ctx context.Context, scope Scope, call llm.ToolCall) (Result, error) {
	r.mu.RLock()
	e, ok := r.tools[call.Name]
	r.mu.RUnlock()
	reject := func(code, format string, a ...any) (Result, error) {
		return Result{}, &CallError{Tool: call.Name, Code: code, Message: fmt.Sprintf(format, a...)}
	}
	if !ok {
		return reject(CodeUnknownTool, "no tool named %q exists", call.Name)
	}
	if scope.AllowedTools != nil && !slices.Contains(scope.AllowedTools, call.Name) {
		return reject(CodeNotAllowed, "this agent may not use %q", call.Name)
	}

	args, doc, err := parseArgs(call.Arguments)
	if err != nil {
		return reject(CodeInvalidArguments, "arguments are not a JSON object: %v", err)
	}
	if err := e.schema.Validate(doc); err != nil {
		return reject(CodeInvalidArguments, "%s", validationMessage(err))
	}
	if err := r.checkScope(ctx, scope, doc); err != nil {
		var ce *CallError
		if errors.As(err, &ce) {
			ce.Tool = call.Name
		}
		return Result{}, err
	}

	tier := e.tool.Tier(args)
	if scope.RequireHITL || tier != domain.Tier1 {
		// Anything but an explicit Tier 1 waits for a person.
		return Result{Tier: domain.Tier2, Proposal: &Proposal{Tool: call.Name, Arguments: args, CallID: call.ID}}, nil
	}
	out, err := e.tool.Execute(ctx, scope, args)
	if err != nil {
		return Result{}, err
	}
	return Result{Tier: domain.Tier1, Output: out}, nil
}

// parseArgs returns the arguments compacted and decoded. Empty arguments
// mean an empty object, as some models send for tools without parameters.
func parseArgs(text string) (json.RawMessage, any, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "{}"
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(text))
	if err != nil {
		return nil, nil, err
	}
	if _, ok := doc.(map[string]any); !ok {
		return nil, nil, errors.New("not an object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(text)); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), doc, nil
}

var english = message.NewPrinter(language.English)

// validationMessage flattens a schema error into one line for the model.
func validationMessage(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	var leaves []string
	var walk func(*jsonschema.ValidationError)
	walk = func(v *jsonschema.ValidationError) {
		if len(v.Causes) == 0 {
			loc := "/" + strings.Join(v.InstanceLocation, "/")
			leaves = append(leaves, fmt.Sprintf("at %s: %s", loc, v.ErrorKind.LocalizedString(english)))
			return
		}
		for _, c := range v.Causes {
			walk(c)
		}
	}
	walk(ve)
	return strings.Join(leaves, "; ")
}

// checkScope verifies every task ID in the arguments.
func (r *Registry) checkScope(ctx context.Context, scope Scope, doc any) error {
	obj := doc.(map[string]any)
	outOfScope := func(id string) error {
		return &CallError{Code: CodeOutOfScope, Message: fmt.Sprintf("task %s is not one this job may use", id)}
	}
	ids := func(keys []string) ([]string, error) {
		var out []string
		for _, k := range keys {
			switch v := obj[k].(type) {
			case nil:
			case string:
				out = append(out, v)
			case []any:
				for _, item := range v {
					s, ok := item.(string)
					if !ok {
						return nil, &CallError{Code: CodeInvalidArguments, Message: k + " must hold task IDs"}
					}
					out = append(out, s)
				}
			default:
				return nil, &CallError{Code: CodeInvalidArguments, Message: k + " must be a task ID"}
			}
		}
		return out, nil
	}
	load := func(raw string) (*domain.Task, error) {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, &CallError{Code: CodeInvalidArguments, Message: fmt.Sprintf("%q is not a task ID", raw)}
		}
		t, err := r.lookup.Get(ctx, scope.TenantID, id)
		if errors.Is(err, domain.ErrNotFound) {
			// The same answer whether the task is missing or in another
			// tenant, so the model learns nothing about other tenants.
			return nil, outOfScope(raw)
		}
		return t, err
	}

	targets, err := ids(targetKeys)
	if err != nil {
		return err
	}
	for _, raw := range targets {
		t, err := load(raw)
		if err != nil {
			return err
		}
		if scope.TaskID == nil {
			continue // a user- or tenant-scoped job: the tenant is the limit
		}
		if t.ID != *scope.TaskID && (t.ParentID == nil || *t.ParentID != *scope.TaskID) {
			return outOfScope(raw)
		}
	}
	refs, err := ids(referenceKeys)
	if err != nil {
		return err
	}
	for _, raw := range refs {
		if _, err := load(raw); err != nil {
			return err
		}
	}
	return nil
}
