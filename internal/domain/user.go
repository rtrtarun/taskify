package domain

// MaxUserIDLen is the longest user ID Taskify accepts.
const MaxUserIDLen = 128

// ValidateUserID returns ErrValidation unless id is an acceptable user ID:
// 1 to 128 printable ASCII characters, space to tilde, so no control
// character and nothing outside ASCII. User IDs appear in the idempotency
// keys Taskify sends AgentPulse, which accept printable ASCII only (AP-11).
// Taskify applies the rule at every identity entry point (the JWT subject,
// the development headers, cmd/devtoken) and wherever a user ID is
// validated: assignee, watchers, creator, comment author, contact owner,
// and a webhook callback's userId. field names the ID in the message, such
// as "assignee_id".
//
// The ID is judged as given. Callers that trim user IDs, as identities and
// task fields always have been, trim first.
func ValidateUserID(field, id string) error {
	if id == "" {
		return invalid("%s is required", field)
	}
	if !userIDOK(id) {
		return invalid("%s must be 1 to %d printable ASCII characters", field, MaxUserIDLen)
	}
	return nil
}

func userIDOK(id string) bool {
	if id == "" || len(id) > MaxUserIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7e {
			return false
		}
	}
	return true
}
