package client

import (
	"strconv"
	"strings"

	"github.com/rtrtarun/taskify/contract"
)

// HeaderAPIVersion is the response header in which the server states the
// version of the API contract it implements.
const HeaderAPIVersion = "X-Taskify-API-Version"

// GeneratedAPIVersion is the API version this package was generated from:
// the info.version of contract/openapi.yaml as compiled in. CI regenerates
// the package whenever the contract changes, so the two move together.
func GeneratedAPIVersion() string { return contract.APIVersion() }

// ServerIsNewer reports whether the server's API version is newer than the
// one this package was generated from, so a caller can suggest an upgrade.
// It is false when either version cannot be read: an unreadable header is
// not a reason to warn.
func ServerIsNewer(serverVersion string) bool {
	return CompareVersions(serverVersion, GeneratedAPIVersion()) > 0
}

// CompareVersions compares two semantic versions (semver.org, section 11):
// major, minor, and patch compare as numbers; a version with a pre-release
// part (1.2.0-rc.1) is lower than the same version without one; pre-release
// identifiers compare one by one, numbers as numbers and below words, words
// in ASCII order, a shorter list below a longer one it begins. Build metadata
// (+abc) and a leading "v" are ignored. It returns -1, 0, or 1, and 0 when
// either side is not a valid version.
func CompareVersions(a, b string) int {
	va, ok := parseVersion(a)
	if !ok {
		return 0
	}
	vb, ok := parseVersion(b)
	if !ok {
		return 0
	}
	for i := range va.nums {
		if c := cmpInt(va.nums[i], vb.nums[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(va.pre) == 0 && len(vb.pre) == 0:
		return 0
	case len(va.pre) == 0:
		return 1
	case len(vb.pre) == 0:
		return -1
	}
	for i := 0; i < len(va.pre) && i < len(vb.pre); i++ {
		if c := cmpIdent(va.pre[i], vb.pre[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(va.pre), len(vb.pre))
}

type semver struct {
	nums [3]int
	pre  []string
}

func parseVersion(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s, _, _ = strings.Cut(s, "+")
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	var v semver
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') || p[0] == '+' {
			return v, false
		}
		v.nums[i] = n
	}
	if hasPre {
		v.pre = strings.Split(pre, ".")
		for _, id := range v.pre {
			if id == "" {
				return v, false
			}
		}
	}
	return v, true
}

func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func cmpIdent(a, b string) int {
	an, bn := isNumeric(a), isNumeric(b)
	switch {
	case an && bn:
		x, _ := strconv.Atoi(a)
		y, _ := strconv.Atoi(b)
		return cmpInt(x, y)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
