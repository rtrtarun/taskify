package e2e

import (
	"encoding/json"
	"testing"

	"github.com/rtrtarun/taskify/contract"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

// checkSentEvents validates every /v1/notify body AgentPulse received
// against the event schema its eventType names (M12-02).
func checkSentEvents(t *testing.T, mock *mockagentpulse.Server) {
	t.Helper()
	for _, r := range mock.RequestsTo("POST", "/v1/notify") {
		var head struct {
			EventType string `json:"eventType"`
		}
		if err := json.Unmarshal(r.Body, &head); err != nil {
			t.Errorf("a /v1/notify body is not JSON: %v", err)
			continue
		}
		if err := contract.ValidateEvent(head.EventType, r.Body); err != nil {
			t.Errorf("sent %s: %v", head.EventType, err)
		}
	}
}
