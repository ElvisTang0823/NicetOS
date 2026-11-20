package main

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed assets/index.html data/blacklist.json data/whitelist.json main.py python/get.py python/hash.py python/requests_loader.py python/requirements.txt all:python_runtime
var bundledAssets embed.FS

func writeBundledAssets(root string) error {
	return fs.WalkDir(bundledAssets, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := bundledAssets.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if strings.HasPrefix(path, "data/") {
			if _, err := os.Stat(target); err == nil {
				return nil
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		return os.WriteFile(target, contents, 0644)
	})
}

func verifyBundledPythonAssets(root string) error {
	required := []string{
		"python_runtime/Lib/site-packages/requests/__init__.py",
		"python_runtime/Lib/site-packages/requests/_internal_utils.py",
		"python_runtime/Lib/site-packages/bs4/__init__.py",
	}
	for _, path := range required {
		expected, err := bundledAssets.ReadFile(path)
		if err != nil {
			return err
		}
		actual, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, expected) {
			return fmt.Errorf("extracted Python package file differs from embedded asset: %s", path)
		}
	}
	return nil
}
