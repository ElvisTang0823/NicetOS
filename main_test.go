package main

import "testing"

func TestPythonInvocationUsesPyLauncher(t *testing.T) {
	cmd, args := choosePythonInvocation("py", "http://example.com")
	if cmd != "py" {
		t.Fatalf("expected py launcher, got %q", cmd)
	}
	if len(args) < 4 || args[0] != "-3" || args[1] != "-c" {
		t.Fatalf("expected py -3 -c invocation, got %#v", args)
	}
}
