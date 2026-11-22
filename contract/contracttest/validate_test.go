package contracttest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const taskBody = `{"id":"0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b","tenant_id":"t1","workspace_id":"ws","title":"x",
"description":"","status":"todo","priority":"p3_medium","creator_id":"u","assignee_id":null,"labels":[],
"metadata":{},"watcher_ids":[],"due_at":null,"remind_at":null,"completed_at":null,"archived_at":null,
"blocked_reason":null,"version":1,"created_at":"2026-11-22T09:00:00Z","updated_at":"2026-11-22T09:00:00Z"}`

// TestValidatorRefusesWhatTheContractDoesNotDescribe is M12-05's guarantee:
// a handler that answers with a status, field, or value the contract does
// not describe fails the test that sees it, and so CI.
func TestValidatorRefusesWhatTheContractDoesNotDescribe(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	json := http.Header{"Content-Type": []string{"application/json"}}
	errBody := `{"error":{"code":"not_found","message":"not found"}}`
	get := httptest.NewRequest("GET", "/api/v1/tasks/0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", nil)

	for name, tc := range map[string]struct {
		req    *http.Request
		status int
		header http.Header
		body   string
		ok     bool
	}{
		"documented task":       {get, 200, json, taskDetail(), true},
		"legacy alias":          {httptest.NewRequest("GET", "/tasks/0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", nil), 200, json, taskDetail(), true},
		"documented error":      {get, 404, json, errBody, true},
		"undocumented status":   {get, 418, json, errBody, false},
		"undocumented field":    {get, 200, json, strings.Replace(taskDetail(), `"version":1`, `"version":1,"secret":"x"`, 1), false},
		"missing field":         {get, 200, json, strings.Replace(taskDetail(), `"version":1,`, ``, 1), false},
		"wrong enum":            {get, 200, json, strings.Replace(taskDetail(), `"todo"`, `"done"`, 1), false},
		"local time":            {get, 200, json, strings.Replace(taskDetail(), `09:00:00Z"`, `10:00:00+01:00"`, 1), false},
		"error without code":    {get, 404, json, `{"error":{"message":"x"}}`, false},
		"wrong content type":    {get, 200, http.Header{"Content-Type": []string{"text/plain"}}, taskDetail(), false},
		"unknown route 404":     {httptest.NewRequest("GET", "/api/v1/nope", nil), 404, json, errBody, true},
		"unknown route 200":     {httptest.NewRequest("GET", "/api/v1/nope", nil), 200, json, `{}`, false},
		"unknown route no body": {httptest.NewRequest("GET", "/api/v1/nope", nil), 404, http.Header{}, `nope`, false},
		"body on 204": {httptest.NewRequest("DELETE", "/api/v1/tasks/0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b?permanent=true", nil),
			204, json, `{}`, false},
		"empty 204": {httptest.NewRequest("DELETE", "/api/v1/tasks/0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b?permanent=true", nil),
			204, http.Header{}, ``, true},
		"suggestions": {httptest.NewRequest("GET", "/api/v1/admin/learning/suggestions", nil), 200, json,
			`{"suggestions":[{"id":"s1","status":"PENDING_APPROVAL","rule_id":null,"event_pattern":{"event_type":"task.updated","label":null},` +
				`"current_adjustment":0,"proposed_adjustment":-5,"signal_count":7,"rationale":"r"}]}`, true},
		"camelCase suggestions": {httptest.NewRequest("GET", "/api/v1/admin/learning/suggestions", nil), 200, json,
			`{"suggestions":[{"id":"s1","status":"PENDING_APPROVAL","suggestion":{"proposedAdjustment":-5}}]}`, false},
	} {
		err := v.Check(tc.req, tc.status, tc.header, []byte(tc.body))
		if tc.ok && err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if err := v.ValidateSchema("Error", map[string]any{"error": map[string]any{"code": "x", "message": "y"}}); err != nil {
		t.Errorf("ValidateSchema: %v", err)
	}
	if err := v.ValidateSchema("Nope", nil); err == nil {
		t.Error("ValidateSchema accepted an unknown schema")
	}
}

func taskDetail() string {
	return strings.TrimSuffix(strings.ReplaceAll(taskBody, "\n", ""), "}") + `,"comments":[]}`
}
