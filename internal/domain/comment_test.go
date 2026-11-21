package domain

import "testing"

func TestIsDecomposeCommand(t *testing.T) {
	for body, want := range map[string]bool{
		"/decompose": true, "  /Decompose\n": true,
		"/decompose please": false, "please /decompose": false, "decompose": false, "": false,
	} {
		if got := IsDecomposeCommand(body); got != want {
			t.Errorf("IsDecomposeCommand(%q) = %v", body, got)
		}
	}
}
