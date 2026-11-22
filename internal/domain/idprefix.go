package domain

import (
	"strings"

	"github.com/google/uuid"
)

// MaxIDPrefixLen is the length of a whole UUID in canonical form.
const MaxIDPrefixLen = 36

// IDPrefixRange returns the smallest and largest UUIDs whose canonical,
// lower-case form starts with prefix, so a list can select tasks by a short
// ID with a range on the primary key (GET /tasks?id_prefix=, M12-04). The
// prefix is 1 to 36 characters of that form, hyphens where the form has
// them; upper case is read as lower. Anything else is ErrValidation.
func IDPrefixRange(prefix string) (lo, hi uuid.UUID, err error) {
	prefix = strings.ToLower(prefix)
	if prefix == "" || len(prefix) > MaxIDPrefixLen {
		return uuid.Nil, uuid.Nil, invalid("id_prefix must be 1 to %d characters", MaxIDPrefixLen)
	}
	const template = "00000000-0000-0000-0000-000000000000"
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		if template[i] == '-' {
			if c != '-' {
				return uuid.Nil, uuid.Nil, invalid("id_prefix must be the start of a UUID such as 0193a1b2-c3d4")
			}
			continue
		}
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return uuid.Nil, uuid.Nil, invalid("id_prefix must be the start of a UUID such as 0193a1b2-c3d4")
		}
	}
	pad := func(fill byte) uuid.UUID {
		b := []byte(template)
		for i := range b {
			switch {
			case i < len(prefix):
				b[i] = prefix[i]
			case b[i] != '-':
				b[i] = fill
			}
		}
		return uuid.MustParse(string(b))
	}
	return pad('0'), pad('f'), nil
}
