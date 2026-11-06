package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestPythonExecutablePrefersBundledRuntime(t *testing.T) {
	root := t.TempDir()
	bundledPython := filepath.Join(root, "python_runtime", "python.exe")
	if err := os.MkdirAll(filepath.Dir(bundledPython), 0755); err != nil {
		t.Fatalf("create bundled Python directory: %v", err)
	}
	if err := os.WriteFile(bundledPython, []byte("bundled runtime placeholder"), 0644); err != nil {
		t.Fatalf("write bundled Python placeholder: %v", err)
	}

	command, args := pythonExecutable(root)
	if command != bundledPython {
		t.Fatalf("expected bundled Python %q, got %q", bundledPython, command)
	}
	if len(args) != 0 {
		t.Fatalf("expected no launcher arguments for bundled Python, got %#v", args)
	}
}

func TestPythonScriptArgsPreserveLauncherArguments(t *testing.T) {
	args := pythonScriptArgs([]string{"-3"}, "C:\\NicetOS\\python\\get.py")
	expected := []string{"-3", "C:\\NicetOS\\python\\get.py"}
	if len(args) != len(expected) {
		t.Fatalf("expected %#v, got %#v", expected, args)
	}
	for i := range expected {
		if args[i] != expected[i] {
			t.Fatalf("expected %#v, got %#v", expected, args)
		}
	}
}

func TestPythonCheckURLUnknownDecisionIsUnknown(t *testing.T) {
	root := t.TempDir()
	scriptPath := filepath.Join(root, "main.py")
	if err := os.WriteFile(scriptPath, []byte("print(0)\n"), 0644); err != nil {
		t.Fatalf("write test script: %v", err)
	}

	allowed, err := pythonCheckURL(root, "https://example.com", "")
	if !errors.Is(err, proxy.ErrUnknownDecision) {
		t.Fatalf("expected unknown decision error, got %v", err)
	}
	if allowed {
		t.Fatal("expected unknown decision 0 to remain unknown, not allow")
	}
}

func TestUnknownDomainIsBlockedWhenAIHasNoDecision(t *testing.T) {
	root := t.TempDir()
	scriptPath := filepath.Join(root, "main.py")
	if err := os.WriteFile(scriptPath, []byte("print(0)\n"), 0644); err != nil {
		t.Fatalf("write test script: %v", err)
	}

	checker := newSiteChecker(root)
	allowed, err := checker.Check("https://example.com")
	if err != nil {
		t.Fatalf("check URL: %v", err)
	}
	if allowed {
		t.Fatal("expected non-whitelisted domain to be blocked without an AI decision")
	}
}

func TestGemmaBlockReasonIsPassedToBlockedPage(t *testing.T) {
	root := t.TempDir()
	scriptPath := filepath.Join(root, "main.py")
	script := `def check_url_with_details(url): return {"decision":"BLOCK","category":"Gambling","confidence":0.93,"reason":"The page promotes betting.","source":"Gemma4"}`
	if err := os.WriteFile(scriptPath, []byte(script+"\n"), 0644); err != nil {
		t.Fatalf("write test script: %v", err)
	}

	checker := newSiteChecker(root)
	allowed, reason, err := checker.CheckWithReason("https://example.com")
	if err != nil {
		t.Fatalf("check URL: %v", err)
	}
	if allowed {
		t.Fatal("expected Gemma BLOCK decision to block the URL")
	}
	for _, expected := range []string{"Gambling", "93%", "The page promotes betting."} {
		if !strings.Contains(reason, expected) {
			t.Errorf("expected block reason to contain %q, got %q", expected, reason)
		}
	}
}

func TestUnknownDomainIsBlockedWhenPythonFails(t *testing.T) {
	root := t.TempDir()
	scriptPath := filepath.Join(root, "main.py")
	if err := os.WriteFile(scriptPath, []byte("raise RuntimeError('inference failed')\n"), 0644); err != nil {
		t.Fatalf("write test script: %v", err)
	}

	checker := newSiteChecker(root)
	allowed, err := checker.Check("https://example.com")
	if err != nil {
		t.Fatalf("check URL: %v", err)
	}
	if allowed {
		t.Fatal("expected non-whitelisted domain to be blocked when Python fails")
	}
}

func TestOllamaEnvironmentOverridesGlobalSettings(t *testing.T) {
	environment := ollamaEnvironment([]string{
		"PATH=C:\\Windows",
		"OLLAMA_HOST=localhost:11434",
		"OLLAMA_MODELS=C:\\Users\\someone\\.ollama\\models",
		"OLLAMA_NO_CLOUD=0",
	}, "127.0.0.1:12345", "E:\\NicetOS\\ollama\\runtime-models")

	for _, entry := range []string{
		"OLLAMA_HOST=127.0.0.1:12345",
		"OLLAMA_MODELS=E:\\NicetOS\\ollama\\runtime-models",
		"OLLAMA_NO_CLOUD=1",
	} {
		if !containsString(environment, entry) {
			t.Errorf("expected environment to contain %q, got %#v", entry, environment)
		}
	}
	for _, entry := range environment {
		if strings.HasPrefix(entry, "OLLAMA_HOST=localhost:") ||
			strings.HasPrefix(entry, "OLLAMA_MODELS=C:\\Users\\") ||
			strings.HasPrefix(entry, "OLLAMA_NO_CLOUD=0") {
			t.Errorf("global Ollama setting leaked into child environment: %q", entry)
		}
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
