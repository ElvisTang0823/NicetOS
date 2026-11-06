package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ollamaBundleTag      = "nicetos-ollama-v0.40.13"
	ollamaBundleVersion  = "ollama-v0.40.13"
	ollamaBundleManifest = "ollama-bundle-manifest.json"
	maxBundleBytes       = int64(16 << 30)
)

var installerBuild = "false"

type ollamaBundlePart struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type ollamaBundleMetadata struct {
	Version      string             `json:"version"`
	ReleaseTag   string             `json:"release_tag"`
	ArchiveName  string             `json:"archive_name"`
	ArchiveSize  int64              `json:"archive_size"`
	ArchiveSHA   string             `json:"archive_sha256"`
	ExpandedSize int64              `json:"expanded_size"`
	Parts        []ollamaBundlePart `json:"parts"`
}

func installApplication() (bool, error) {
	if installerBuild != "true" {
		return false, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return false, errors.New("LOCALAPPDATA is not set")
	}
	installDir := filepath.Join(localAppData, "Programs", "NicetOS")
	installedExecutable := filepath.Join(installDir, "NicetOS.exe")
	if samePath(executable, installedExecutable) {
		return false, nil
	}

	log.Printf("[INSTALL] Installing NicetOS for the current user to %s", installDir)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return false, fmt.Errorf("create AppData install directory: %w", err)
	}
	if err := writeBundledAssets(installDir); err != nil {
		return false, fmt.Errorf("extract embedded application files: %w", err)
	}
	if err := copyExecutable(executable, installedExecutable); err != nil {
		return false, fmt.Errorf("install NicetOS executable: %w", err)
	}
	if err := installOllamaBundle(installDir); err != nil {
		return false, fmt.Errorf("install Ollama runtime from GitHub Releases: %w", err)
	}
	if err := createStartMenuShortcut(installedExecutable); err != nil {
		return false, fmt.Errorf("create Start menu shortcut: %w", err)
	}
	log.Printf("[INSTALL] Installation completed. Start NicetOS from the Start menu; this installer did not change proxy settings.")
	return true, nil
}

func samePath(first string, second string) bool {
	firstAbs, firstErr := filepath.Abs(first)
	secondAbs, secondErr := filepath.Abs(second)
	return firstErr == nil && secondErr == nil && strings.EqualFold(filepath.Clean(firstAbs), filepath.Clean(secondAbs))
}

func copyExecutable(source string, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary := destination + ".new"
	output, err := os.Create(temporary)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(temporary)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(temporary)
		return closeErr
	}
	backup := destination + ".backup"
	hadPrevious := false
	if _, err := os.Stat(destination); err == nil {
		if _, backupErr := os.Stat(backup); backupErr == nil {
			_ = os.Remove(temporary)
			return fmt.Errorf("previous install backup already exists: %s", backup)
		} else if !os.IsNotExist(backupErr) {
			_ = os.Remove(temporary)
			return backupErr
		}
		if err := os.Rename(destination, backup); err != nil {
			_ = os.Remove(temporary)
			return fmt.Errorf("preserve existing executable: %w", err)
		}
		hadPrevious = true
	} else if !os.IsNotExist(err) {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Remove(temporary)
		if hadPrevious {
			_ = os.Rename(backup, destination)
		}
		return err
	}
	if hadPrevious {
		_ = os.Remove(backup)
	}
	return nil
}

func createStartMenuShortcut(executable string) error {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return errors.New("APPDATA is not set")
	}
	shortcut := filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "NicetOS.cmd")
	if err := os.MkdirAll(filepath.Dir(shortcut), 0755); err != nil {
		return err
	}
	content := "@echo off\r\nstart \"NicetOS\" \"%LOCALAPPDATA%\\Programs\\NicetOS\\NicetOS.exe\"\r\n"
	return os.WriteFile(shortcut, []byte(content), 0644)
}

func installOllamaBundle(installDir string) error {
	ollamaDir := filepath.Join(installDir, "ollama")
	markerPath := filepath.Join(ollamaDir, ".bundle-version")
	if marker, err := os.ReadFile(markerPath); err == nil {
		if strings.TrimSpace(string(marker)) == ollamaBundleVersion && verifyOllamaBundle(ollamaDir) == nil {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read installed Ollama version: %w", err)
	}

	manifestURL := fmt.Sprintf("https://github.com/ElvisTang0823/NicetOS/releases/download/%s/%s",
		ollamaBundleTag, ollamaBundleManifest)
	log.Printf("[INSTALL] Downloading Ollama runtime and model from GitHub Release %s. This is several gigabytes.", ollamaBundleTag)
	metadata, err := downloadBundleMetadata(manifestURL)
	if err != nil {
		return err
	}
	if metadata.Version != ollamaBundleVersion || metadata.ReleaseTag != ollamaBundleTag {
		return fmt.Errorf("unexpected bundle version/tag %q/%q", metadata.Version, metadata.ReleaseTag)
	}
	if err := validateBundleMetadata(metadata); err != nil {
		return err
	}

	temporary, err := os.MkdirTemp(installDir, ".ollama-install-*")
	if err != nil {
		return fmt.Errorf("create temporary installation directory: %w", err)
	}
	defer os.RemoveAll(temporary)

	archivePath := filepath.Join(temporary, metadata.ArchiveName)
	if err := downloadBundleArchive(manifestURL, metadata, archivePath); err != nil {
		return err
	}
	extractDir := filepath.Join(temporary, "extracted")
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return err
	}
	if err := extractOllamaArchive(archivePath, extractDir, metadata.ExpandedSize); err != nil {
		return err
	}
	if err := verifyOllamaBundle(filepath.Join(extractDir, "ollama")); err != nil {
		return err
	}
	if err := replaceOllamaDirectories(filepath.Join(extractDir, "ollama"), ollamaDir, temporary); err != nil {
		return err
	}
	if err := os.WriteFile(markerPath, []byte(ollamaBundleVersion+"\n"), 0644); err != nil {
		return err
	}
	return nil
}

func downloadBundleMetadata(manifestURL string) (ollamaBundleMetadata, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return ollamaBundleMetadata{}, err
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return ollamaBundleMetadata{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ollamaBundleMetadata{}, fmt.Errorf("download release manifest: HTTP %s", response.Status)
	}
	var metadata ollamaBundleMetadata
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&metadata); err != nil {
		return ollamaBundleMetadata{}, fmt.Errorf("decode release manifest: %w", err)
	}
	return metadata, nil
}

func validateBundleMetadata(metadata ollamaBundleMetadata) error {
	if metadata.ArchiveName != "ollama-bundle.tar.gz" {
		return fmt.Errorf("invalid bundle archive name %q", metadata.ArchiveName)
	}
	if metadata.ArchiveSize <= 0 || metadata.ArchiveSize > maxBundleBytes ||
		metadata.ExpandedSize <= 0 || metadata.ExpandedSize > maxBundleBytes {
		return errors.New("bundle size is invalid or exceeds the supported limit")
	}
	if len(metadata.Parts) == 0 || len(metadata.Parts) > 16 {
		return errors.New("release manifest must list between 1 and 16 archive parts")
	}
	var total int64
	seen := make(map[string]struct{}, len(metadata.Parts))
	for index, part := range metadata.Parts {
		if filepath.Base(part.Name) != part.Name || part.Name == "." || part.Name == ".." ||
			strings.ContainsAny(part.Name, `/\?#`) || !strings.HasPrefix(part.Name, "ollama-bundle.tar.gz.part") {
			return fmt.Errorf("invalid release asset name %q", part.Name)
		}
		expectedName := fmt.Sprintf("ollama-bundle.tar.gz.part%03d", index+1)
		if part.Name != expectedName {
			return fmt.Errorf("unexpected release asset name %q; expected %q", part.Name, expectedName)
		}
		if _, exists := seen[part.Name]; exists {
			return fmt.Errorf("duplicate release asset name %q", part.Name)
		}
		seen[part.Name] = struct{}{}
		if part.Size <= 0 || part.Size >= 2<<30 {
			return fmt.Errorf("release asset %q must be between 1 byte and 2 GiB", part.Name)
		}
		if _, err := hex.DecodeString(part.SHA256); err != nil || len(part.SHA256) != sha256.Size*2 {
			return fmt.Errorf("invalid SHA-256 for release asset %q", part.Name)
		}
		total += part.Size
	}
	if total != metadata.ArchiveSize {
		return fmt.Errorf("archive parts total %d bytes, expected %d", total, metadata.ArchiveSize)
	}
	if _, err := hex.DecodeString(metadata.ArchiveSHA); err != nil || len(metadata.ArchiveSHA) != sha256.Size*2 {
		return errors.New("invalid archive SHA-256")
	}
	return nil
}

func downloadBundleArchive(manifestURL string, metadata ollamaBundleMetadata, archivePath string) error {
	baseURL := strings.TrimSuffix(manifestURL, ollamaBundleManifest)
	archive, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	archiveHash := sha256.New()
	client := &http.Client{Timeout: time.Hour}
	var total int64
	for index, part := range metadata.Parts {
		url := baseURL + part.Name
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if requestErr != nil {
			cancel()
			_ = archive.Close()
			return requestErr
		}
		response, requestErr := client.Do(request)
		if requestErr != nil {
			cancel()
			_ = archive.Close()
			return fmt.Errorf("download release asset %s: %w", part.Name, requestErr)
		}
		if response.StatusCode != http.StatusOK {
			status := response.Status
			_ = response.Body.Close()
			cancel()
			_ = archive.Close()
			return fmt.Errorf("download release asset %s: HTTP %s", part.Name, status)
		}
		partHash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(archive, archiveHash, partHash), io.LimitReader(response.Body, part.Size+1))
		bodyCloseErr := response.Body.Close()
		cancel()
		if copyErr != nil {
			_ = archive.Close()
			return fmt.Errorf("write release asset %s: %w", part.Name, copyErr)
		}
		if bodyCloseErr != nil {
			_ = archive.Close()
			return bodyCloseErr
		}
		if written != part.Size {
			_ = archive.Close()
			return fmt.Errorf("release asset %s has size %d, expected %d", part.Name, written, part.Size)
		}
		if !strings.EqualFold(hex.EncodeToString(partHash.Sum(nil)), part.SHA256) {
			_ = archive.Close()
			return fmt.Errorf("release asset %s failed SHA-256 verification", part.Name)
		}
		total += written
		log.Printf("[INSTALL] Downloaded part %d/%d (%d bytes)", index+1, len(metadata.Parts), written)
	}
	closeErr := archive.Close()
	if closeErr != nil {
		return closeErr
	}
	if total != metadata.ArchiveSize || !strings.EqualFold(hex.EncodeToString(archiveHash.Sum(nil)), metadata.ArchiveSHA) {
		return errors.New("downloaded Ollama archive failed size or SHA-256 verification")
	}
	return nil
}

func extractOllamaArchive(archivePath string, destination string, expectedSize int64) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open Ollama archive: %w", err)
	}
	defer compressed.Close()

	reader := tar.NewReader(compressed)
	var extracted int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read Ollama archive: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if filepath.IsAbs(name) || name == "." || name == ".." ||
			strings.HasPrefix(name, ".."+string(filepath.Separator)) ||
			(name != "ollama" && !strings.HasPrefix(name, "ollama"+string(filepath.Separator))) {
			return fmt.Errorf("invalid path in Ollama archive: %q", header.Name)
		}
		target := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			extracted += header.Size
			if header.Size < 0 || extracted > expectedSize || extracted > maxBundleBytes {
				return errors.New("Ollama archive exceeds the expected expanded size")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(output, reader, header.Size)
			closeErr := output.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported file type in Ollama archive: %q", header.Name)
		}
	}
	if extracted != expectedSize {
		return fmt.Errorf("expanded Ollama archive size is %d bytes, expected %d", extracted, expectedSize)
	}
	return nil
}

func verifyOllamaBundle(root string) error {
	for _, path := range []string{
		filepath.Join(root, "bin", "ollama.exe"),
		filepath.Join(root, "models", "Modelfile"),
		filepath.Join(root, "models", "gemma4_child_safety_q4.gguf"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("bundle is missing required file %s: %w", path, err)
		}
		if info.IsDir() {
			return fmt.Errorf("bundle path is a directory: %s", path)
		}
	}
	return nil
}

func replaceOllamaDirectories(source string, destination string, temporary string) error {
	if err := os.MkdirAll(destination, 0755); err != nil {
		return err
	}
	type replacement struct {
		destination string
		backup      string
		hadPrevious bool
		installed   bool
	}
	var replacements []replacement
	rollback := func() {
		for i := len(replacements) - 1; i >= 0; i-- {
			item := replacements[i]
			if item.installed {
				_ = os.RemoveAll(item.destination)
			}
			if item.hadPrevious {
				_ = os.Rename(item.backup, item.destination)
			}
		}
	}
	for _, name := range []string{"bin", "models"} {
		sourcePath := filepath.Join(source, name)
		destinationPath := filepath.Join(destination, name)
		backupPath := filepath.Join(temporary, "previous-"+name)
		item := replacement{destination: destinationPath, backup: backupPath}
		if _, err := os.Stat(destinationPath); err == nil {
			if err := os.Rename(destinationPath, backupPath); err != nil {
				rollback()
				return fmt.Errorf("preserve existing Ollama %s: %w", name, err)
			}
			item.hadPrevious = true
		} else if !os.IsNotExist(err) {
			rollback()
			return err
		}
		replacements = append(replacements, item)
		if err := os.Rename(sourcePath, destinationPath); err != nil {
			rollback()
			return fmt.Errorf("install Ollama %s: %w", name, err)
		}
		replacements[len(replacements)-1].installed = true
	}
	return nil
}
