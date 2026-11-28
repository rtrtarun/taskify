//go:build !webembed

package web

import "testing"

func TestStubHasNoFrontend(t *testing.T) {
	if fsys, ok := Dist(); ok || fsys != nil {
		t.Fatal("the plain build must not claim to hold a frontend")
	}
}
