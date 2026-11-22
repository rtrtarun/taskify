package domain

import (
	"encoding/json"
	"testing"
)

func TestEventPattern(t *testing.T) {
	docs, docs2, ops := "docs", "docs", "ops"
	for _, tc := range []struct {
		a, b  EventPattern
		equal bool
	}{
		{EventPattern{"task.updated", &docs}, EventPattern{"task.updated", &docs2}, true},
		{EventPattern{"task.updated", nil}, EventPattern{"task.updated", nil}, true},
		{EventPattern{"task.updated", &docs}, EventPattern{"task.updated", &ops}, false},
		{EventPattern{"task.updated", &docs}, EventPattern{"task.updated", nil}, false},
		{EventPattern{"task.updated", nil}, EventPattern{"task.created", nil}, false},
	} {
		if tc.a.Equal(tc.b) != tc.equal || tc.b.Equal(tc.a) != tc.equal {
			t.Errorf("%s = %s: %v, want %v", tc.a, tc.b, tc.a.Equal(tc.b), tc.equal)
		}
	}
	// AgentPulse's JSON: label null, never left out, for no label.
	raw, _ := json.Marshal(EventPattern{EventType: "task.updated"})
	if string(raw) != `{"eventType":"task.updated","label":null}` {
		t.Errorf("JSON = %s", raw)
	}
	if s := (EventPattern{"task.updated", &docs}).String(); s != "task.updated, label docs" {
		t.Errorf("String = %q", s)
	}
}
