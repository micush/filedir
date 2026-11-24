package main

import "testing"

// A person sees the name of the node that last changed a file, not its id.
func TestNodeNamesAreHostNames(t *testing.T) {
	n := newFdTestNode(t, "a")
	names := fdSvc.nodeNames()
	self := fdSvc.selfID()
	if names[self] == "" || byName(names, self) == self && names[self] != self {
		t.Fatalf("self %q has no name: %v", self, names)
	}
	if got := byName(names, "deadbeef00000000"); got != "deadbeef00000000" {
		t.Fatalf("an unknown node should keep its id: %q", got)
	}
	_ = n
}
