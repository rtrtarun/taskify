package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rtrtarun/taskify/contract"
	"github.com/rtrtarun/taskify/internal/repository/repotest"
)

// productionTools are the tools the service registers (internal/app).
func productionTools() []Tool {
	return []Tool{UpdateTaskAttributes{}, LinkDependencies{}, CreateSubtaskDAG{}, DraftResolutionComment{},
		ReassignTask{}, ShiftDueDate{}, ArchiveTask{}, ProposeNotificationRule{}}
}

// TestToolsTakeTheirSchemasFromTheContract is M12-02: a tool has one
// definition, contract/tools/{name}.schema.json, which the registry loads
// and offers to the model unchanged; and the contract defines no tool the
// service lacks.
func TestToolsTakeTheirSchemasFromTheContract(t *testing.T) {
	reg := NewRegistry(repotest.Memory().Tasks)
	var names []string
	for _, tool := range productionTools() {
		if _, self := tool.(SelfDescribed); self {
			t.Errorf("%s carries its own schema", tool.Name())
		}
		if err := reg.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
		names = append(names, tool.Name())
	}
	schemas, err := reg.Schemas(names)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range schemas {
		want, _ := contract.ToolSchema(s.Name)
		if !bytes.Equal(s.Parameters, want) {
			t.Errorf("%s: offered schema differs from contract/tools", s.Name)
		}
	}
	slices.Sort(names)
	if got := contract.ToolNames(); !slices.Equal(got, names) {
		t.Errorf("contract/tools defines %v; the service has %v", got, names)
	}
}

// missingTool is a production-style tool, without its own schema, that the
// contract does not define.
type missingTool struct{ UpdateTaskAttributes }

func (missingTool) Name() string { return "Teleport" }

// selfDefined is a contract tool that also carries a schema of its own.
type selfDefined struct{ ArchiveTask }

func (selfDefined) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

// TestRegistryRefusesAToolWithoutOneSchema: the service does not start with
// a tool whose schema is missing from the contract, or defined twice.
func TestRegistryRefusesAToolWithoutOneSchema(t *testing.T) {
	reg := NewRegistry(repotest.Memory().Tasks)
	err := reg.Register(missingTool{})
	if !errors.Is(err, contract.ErrNoSchema) || !strings.Contains(err.Error(), "contract/tools/Teleport.schema.json") {
		t.Errorf("missing schema: %v", err)
	}
	if err := reg.Register(selfDefined{}); err == nil || !strings.Contains(err.Error(), "must not carry its own schema") {
		t.Errorf("two schemas: %v", err)
	}
	// Neither was registered.
	if _, err := reg.Schemas([]string{"Teleport"}); err == nil {
		t.Error("Teleport was registered")
	}
	if _, err := reg.Dispatch(context.Background(), Scope{TenantID: "t"}, toolCall(ArchiveTaskName, `{}`)); !errors.Is(err, ErrRejected) {
		t.Errorf("ArchiveTask was registered: %v", err)
	}
}
