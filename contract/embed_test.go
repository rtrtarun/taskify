package contract

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestSchemasCompile: every tool and event schema in the contract is valid
// JSON Schema within the subset the validator supports (D22), so no rule in
// it is silently unenforced.
func TestSchemasCompile(t *testing.T) {
	tools := ToolNames()
	wantTools := []string{"ArchiveTask", "CreateSubtaskDAG", "DraftResolutionComment", "LinkDependencies",
		"ProposeNotificationRule", "ReassignTask", "ShiftDueDate", "UpdateTaskAttributes"}
	if !slices.Equal(tools, wantTools) {
		t.Errorf("tools = %v, want %v", tools, wantTools)
	}
	for _, name := range tools {
		raw, err := ToolSchema(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := CompileSchema(raw); err != nil {
			t.Errorf("tools/%s: %v", name, err)
		}
		// A tool's schema is sent to the model as it is: no keyword a
		// provider might refuse, even an annotation the subset allows.
		root, _ := DecodeJSON(raw)
		if m, _ := root.(map[string]any); m["$schema"] != nil || m["title"] != nil {
			t.Errorf("tools/%s carries $schema or title", name)
		}
	}
	events := EventTypes()
	if len(events) != 19 {
		t.Errorf("events = %v, want 16 notification types, contact.update, learning.suggestion, and feedback.forward", events)
	}
	for _, typ := range events {
		raw, err := EventSchema(typ)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := CompileSchema(raw); err != nil {
			t.Errorf("events/%s: %v", typ, err)
		}
		if !strings.Contains(string(raw), `"title": "`+typ+`"`) {
			t.Errorf("events/%s: title does not name the event", typ)
		}
	}
}

func TestSchemaLookups(t *testing.T) {
	for _, name := range []string{"", "Nope", "../openapi", `a\b`, "tools/ArchiveTask"} {
		if _, err := ToolSchema(name); !errors.Is(err, ErrNoSchema) {
			t.Errorf("ToolSchema(%q) = %v, want ErrNoSchema", name, err)
		}
	}
	if _, err := EventSchema("task.nope"); !errors.Is(err, ErrNoSchema) {
		t.Errorf("EventSchema = %v", err)
	}
	if err := ValidateEvent("task.nope", []byte(`{}`)); !errors.Is(err, ErrNoSchema) {
		t.Errorf("ValidateEvent of an unknown type = %v", err)
	}
	if err := ValidateEvent("contact.update", []byte(`{`)); err == nil {
		t.Error("ValidateEvent accepted malformed JSON")
	}
	ok := `{"userId": "usr_bob", "email": "bob@example.com", "slackUserId": null, "phone": null}`
	if err := ValidateEvent("contact.update", []byte(ok)); err != nil {
		t.Errorf("valid contact: %v", err)
	}
	bad := `{"userId": "usr_bob", "email": 5, "slackUserId": null}`
	err := ValidateEvent("contact.update", []byte(bad))
	if err == nil || !strings.Contains(err.Error(), `missing property "phone"`) || !strings.Contains(err.Error(), "/email") {
		t.Errorf("invalid contact: %v", err)
	}
}

// TestR2ChecklistIsEmpty is M12-08: contract/TODO.md lists the R2 items
// missing from the contract, and R2 is complete.
func TestR2ChecklistIsEmpty(t *testing.T) {
	raw, err := os.ReadFile("TODO.md")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "- [ ]") {
		t.Error("contract/TODO.md has an unticked item")
	}
	if !strings.Contains(string(raw), "## Open\n\nNone.") {
		t.Error("contract/TODO.md's open list is not empty")
	}
}
