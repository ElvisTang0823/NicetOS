package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	proxy "nicetos/go"
)

func TestPythonInvocationUsesPyLauncher(t *testing.T) {
	cmd, args := choosePythonInvocation("py", "http://example.com")
	if cmd != "py" {
		t.Fatalf("expected py launcher, got %q", cmd)
	}
	if len(args) < 4 || args[0] != "-3" || args[1] != "-c" {
		t.Fatalf("expected py -3 -c invocation, got %#v", args)
	}
}

func TestPythonCheckURLUnknownDecisionIsUnknown(t *testing.T) {
	root := t.TempDir()
	scriptPath := filepath.Join(root, "main.py")
	if err := os.WriteFile(scriptPath, []byte("print(0)\n"), 0644); err != nil {
		t.Fatalf("write test script: %v", err)
	}

	allowed, err := pythonCheckURL(root, "https://example.com")
	if !errors.Is(err, proxy.ErrUnknownDecision) {
		t.Fatalf("expected unknown decision error, got %v", err)
	}
	if allowed {
		t.Fatal("expected unknown decision 0 to remain unknown, not allow")
	}
}
