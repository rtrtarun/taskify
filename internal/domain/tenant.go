package domain

import "regexp"

// TenantIDPattern is the rule for tenant IDs, the one AgentPulse enforces
// (docs/blockers.md, "Agreed with AgentPulse"): a letter or digit, then up to
// 63 letters, digits, dots, underscores, or hyphens. Taskify applies it at
// every entry point (the JWT claim, the development headers, cmd/devtoken)
// and in validation, so no tenant can exist in Taskify that AgentPulse would
// refuse (AP-04).
const TenantIDPattern = `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`

// ReservedTenantID is refused: AgentPulse keeps it for itself.
const ReservedTenantID = "default"

var tenantIDRe = regexp.MustCompile(TenantIDPattern)

// ValidateTenantID returns ErrValidation unless id is an acceptable tenant
// ID. The ID is judged exactly as given: it is never trimmed, and case
// matters ("Acme" and "acme" are two tenants, and only "default" itself is
// reserved).
func ValidateTenantID(id string) error {
	switch {
	case id == "":
		return invalid("tenant_id is required")
	case id == ReservedTenantID:
		return invalid("tenant_id %q is reserved", id)
	case !tenantIDRe.MatchString(id):
		return invalid("tenant_id must match %s", TenantIDPattern)
	}
	return nil
}
