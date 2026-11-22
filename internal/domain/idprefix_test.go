package domain

import (
	"errors"
	"testing"
)

func TestIDPrefixRange(t *testing.T) {
	for prefix, want := range map[string][2]string{
		"0":                                    {"00000000-0000-0000-0000-000000000000", "0fffffff-ffff-ffff-ffff-ffffffffffff"},
		"0193A1B2":                             {"0193a1b2-0000-0000-0000-000000000000", "0193a1b2-ffff-ffff-ffff-ffffffffffff"},
		"0193a1b2-":                            {"0193a1b2-0000-0000-0000-000000000000", "0193a1b2-ffff-ffff-ffff-ffffffffffff"},
		"0193a1b2-c3":                          {"0193a1b2-c300-0000-0000-000000000000", "0193a1b2-c3ff-ffff-ffff-ffffffffffff"},
		"0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b": {"0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"},
	} {
		lo, hi, err := IDPrefixRange(prefix)
		if err != nil || lo.String() != want[0] || hi.String() != want[1] {
			t.Errorf("%q: %s..%s, %v", prefix, lo, hi, err)
		}
	}
	for _, bad := range []string{"", "0193a1b2c3", "xyz", "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b0", "0193a1b2_", "-"} {
		if _, _, err := IDPrefixRange(bad); !errors.Is(err, ErrValidation) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
