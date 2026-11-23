package client

import (
	"fmt"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.1.0", "1.0.9", 1},
		{"2.0.0", "1.99.99", 1},
		{"1.10.0", "1.9.0", 1}, // numbers, not text
		{"1.0.0", "1.0.1", -1},
		{"v1.2.0", "1.2.0", 0},
		{"1.2.0+build5", "1.2.0+build9", 0},
		// Pre-releases, in the order semver.org gives.
		{"1.0.0-alpha", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1},
		{"1.0.0-alpha.beta", "1.0.0-beta", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-beta.11", "1.0.0-rc.1", -1},
		{"1.1.0-rc.1", "1.0.0", 1},
		// Not versions: no ordering.
		{"", "1.0.0", 0},
		{"1.0.0", "", 0},
		{"1.0", "1.0.0", 0},
		{"1.0.0.0", "1.0.0", 0},
		{"1.x.0", "1.0.0", 0},
		{"01.0.0", "1.0.0", 0},
		{"-1.0.0", "1.0.0", 0},
		{"1.0.0-", "1.0.0", 0},
		{"1.0.0-a..b", "1.0.0", 0},
	} {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if tc.want != 0 {
			if got := CompareVersions(tc.b, tc.a); got != -tc.want {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d (the reverse)", tc.b, tc.a, got, -tc.want)
			}
		}
	}
}

func TestServerIsNewer(t *testing.T) {
	g := GeneratedAPIVersion()
	if g == "" {
		t.Fatal("no generated version")
	}
	next := func(v string) string { // the same version with the patch raised
		var a, b, c int
		if _, err := fmt.Sscanf(v, "%d.%d.%d", &a, &b, &c); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%d.%d.%d", a, b, c+1)
	}
	if ServerIsNewer(g) || ServerIsNewer("") || ServerIsNewer("garbage") || ServerIsNewer("0.0.1") {
		t.Errorf("a same, missing, unreadable, or older server is not newer than %s", g)
	}
	if !ServerIsNewer(next(g)) || !ServerIsNewer("99.0.0") {
		t.Errorf("a newer server is newer than %s", g)
	}
}
