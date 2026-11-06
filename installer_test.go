package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

func TestFormatBundleBytes(t *testing.T) {
	tests := []struct {
		size int64
		want string
	}{
		{size: 0, want: "0 B"},
		{size: 1024, want: "1.0 KiB"},
		{size: 5 << 30, want: "5.0 GiB"},
	}
	for _, test := range tests {
		if got := formatBundleBytes(test.size); got != test.want {
			t.Errorf("formatBundleBytes(%d) = %q, want %q", test.size, got, test.want)
		}
	}
}

func TestDownloadBundleArchiveDownloadsPartsConcurrently(t *testing.T) {
	payloads := map[string][]byte{
		"ollama-bundle.tar.gz.part001": []byte("first part"),
		"ollama-bundle.tar.gz.part002": []byte("second part"),
	}
	var active atomic.Int32
	var maximumActive atomic.Int32
	bothStarted := make(chan struct{})
	var startOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for maximum := maximumActive.Load(); current > maximum; maximum = maximumActive.Load() {
			if maximumActive.CompareAndSwap(maximum, current) {
				break
			}
		}
		if current >= 2 {
			startOnce.Do(func() { close(bothStarted) })
		}
		select {
		case <-bothStarted:
		case <-request.Context().Done():
			return
		}
		_, _ = response.Write(payloads[filepath.Base(request.URL.Path)])
	}))
	defer server.Close()

	first := payloads["ollama-bundle.tar.gz.part001"]
	second := payloads["ollama-bundle.tar.gz.part002"]
	archiveContents := append(append([]byte(nil), first...), second...)
	partMetadata := func(name string, data []byte) ollamaBundlePart {
		hash := sha256.Sum256(data)
		return ollamaBundlePart{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
	}
	archiveHash := sha256.Sum256(archiveContents)
	metadata := ollamaBundleMetadata{
		ArchiveName: "ollama-bundle.tar.gz",
		ArchiveSize: int64(len(archiveContents)),
		ArchiveSHA:  hex.EncodeToString(archiveHash[:]),
		Parts: []ollamaBundlePart{
			partMetadata("ollama-bundle.tar.gz.part001", first),
			partMetadata("ollama-bundle.tar.gz.part002", second),
		},
	}
	archivePath := filepath.Join(t.TempDir(), metadata.ArchiveName)
	manifestURL := fmt.Sprintf("%s/%s", server.URL, ollamaBundleManifest)
	if err := downloadBundleArchive(manifestURL, metadata, archivePath); err != nil {
		t.Fatalf("download and assemble bundle parts: %v", err)
	}
	if maximumActive.Load() < 2 {
		t.Fatalf("expected concurrent part downloads, maximum active requests was %d", maximumActive.Load())
	}
	got, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("read assembled archive: %v", err)
	}
	if string(got) != string(archiveContents) {
		t.Fatalf("assembled archive = %q, want %q", got, archiveContents)
	}
	for name := range payloads {
		if _, err := os.Stat(filepath.Join(filepath.Dir(archivePath), name)); !os.IsNotExist(err) {
			t.Errorf("temporary part %s was not removed after assembly", name)
		}
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
