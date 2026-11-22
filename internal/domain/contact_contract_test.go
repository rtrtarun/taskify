package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rtrtarun/taskify/internal/domain"
	"github.com/rtrtarun/taskify/tests/mockagentpulse"
)

// TestContactFormatsMatchAgentPulse is AP-12: Taskify's contact validation
// is exactly AgentPulse's, as the mock applies it, so a value Taskify
// accepts is never refused when it is forwarded, and one AgentPulse would
// refuse is refused when the person enters it.
func TestContactFormatsMatchAgentPulse(t *testing.T) {
	candidates := map[string][]string{
		"email": {"bob@example.com", "b@x.io", "first.last+tag@sub.example.co", "Bob <bob@example.com>", "bob", "@example.com",
			"bob@", "bob@@example.com", strings.Repeat("b", 242) + "@example.com", strings.Repeat("b", 243) + "@example.com"},
		"slackUserId": {"U1", "W1", "U024BE7LH", "W" + strings.Repeat("A", 31), "U" + strings.Repeat("A", 32), "U", "W",
			"u024be7lh", "C024BE7LH", "U024-BE7LH", "UÄ"},
		"phone": {"+12", "+14155550123", "+" + "1" + strings.Repeat("2", 14), "+" + "1" + strings.Repeat("2", 15), "+1", "+0123",
			"14155550123", "+1-415-555-0123", "+1 415", "++14155550123"},
	}
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	for field, values := range candidates {
		for _, v := range values {
			value := v
			var p domain.ContactPatch
			switch field {
			case "email":
				p.Email = domain.ContactField{Set: true, Value: &value}
			case "slackUserId":
				p.SlackUserID = domain.ContactField{Set: true, Value: &value}
			case "phone":
				p.Phone = domain.ContactField{Set: true, Value: &value}
			}
			_, _, taskifyErr := domain.Contact{}.Apply(p, now)
			agentPulseErr := mockagentpulse.CheckContact(map[string]any{field: v})
			if (taskifyErr == nil) != (agentPulseErr == nil) {
				t.Errorf("%s %q: Taskify %v, AgentPulse %v", field, v, taskifyErr, agentPulseErr)
			}
			if taskifyErr != nil && !errors.Is(taskifyErr, domain.ErrValidation) {
				t.Errorf("%s %q: %v is not a validation error", field, v, taskifyErr)
			}
			if agentPulseErr != nil && !strings.Contains(agentPulseErr.Error(), field) {
				t.Errorf("%s %q: AgentPulse's message %q does not name the field", field, v, agentPulseErr)
			}
		}
	}
	// One bad field refuses the whole body.
	if err := mockagentpulse.CheckContact(map[string]any{"email": "bob@example.com", "phone": "x"}); err == nil ||
		!strings.Contains(err.Error(), "phone") {
		t.Errorf("mixed body: %v", err)
	}
}
