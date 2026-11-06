package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateBundleMetadata(t *testing.T) {
	hash := hex.EncodeToString(make([]byte, sha256.Size))
	metadata := ollamaBundleMetadata{
		Version:      ollamaBundleVersion,
		ReleaseTag:   ollamaBundleTag,
		ArchiveName:  "ollama-bundle.tar.gz",
		ArchiveSize:  1,
		ArchiveSHA:   hash,
		ExpandedSize: 1,
		Parts: []ollamaBundlePart{{
			Name:   "ollama-bundle.tar.gz.part001",
			Size:   1,
			SHA256: hash,
		}},
	}
	if err := validateBundleMetadata(metadata); err != nil {
		t.Fatalf("valid bundle metadata rejected: %v", err)
	}

	metadata.Parts[0].Name = "..\\payload"
	if err := validateBundleMetadata(metadata); err == nil {
		t.Fatal("expected invalid release asset name to be rejected")
	}
}

func TestReplaceOllamaDirectoriesRollsBackOnFailure(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "new")
	destination := filepath.Join(root, "installed", "ollama")
	temporary := filepath.Join(root, "temporary")
	for _, directory := range []string{
		filepath.Join(source, "bin"),
		filepath.Join(destination, "bin"),
		filepath.Join(destination, "models"),
		temporary,
	} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile := func(path, contents string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(filepath.Join(source, "bin", "ollama.exe"), "new")
	writeTestFile(filepath.Join(destination, "bin", "ollama.exe"), "old")
	writeTestFile(filepath.Join(destination, "models", "model.gguf"), "preserve")

	if err := replaceOllamaDirectories(source, destination, temporary); err == nil {
		t.Fatal("expected missing staged models directory to fail")
	}
	bin, err := os.ReadFile(filepath.Join(destination, "bin", "ollama.exe"))
	if err != nil {
		t.Fatalf("read rolled-back executable: %v", err)
	}
	if string(bin) != "old" {
		t.Fatalf("executable was not restored after failure: got %q", bin)
	}
	model, err := os.ReadFile(filepath.Join(destination, "models", "model.gguf"))
	if err != nil {
		t.Fatalf("read preserved model: %v", err)
	}
	if string(model) != "preserve" {
		t.Fatalf("existing model changed after rollback: got %q", model)
	}
}
