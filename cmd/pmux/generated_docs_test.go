package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/0p9b/pmux/cmd/pmux/internaldocs"
	"github.com/spf13/cobra"
)

func TestGeneratedCommandDocsHaveNoDrift(t *testing.T) {
	repository := testModuleRoot(t)
	temporary := t.TempDir()
	generatedReference := filepath.Join(temporary, "reference")
	generatedMan := filepath.Join(temporary, "man1")
	if err := internaldocs.Generate(func() *cobra.Command {
		return newRootCommand(testDependencies(&commandSpy{}, false, &bytes.Buffer{}, &bytes.Buffer{}))
	}, generatedReference, generatedMan); err != nil {
		t.Fatalf("generate command documentation: %v", err)
	}

	compareGeneratedTree(t, generatedReference, filepath.Join(repository, "docs", "reference", "commands"))
	compareGeneratedTree(t, generatedMan, filepath.Join(repository, "docs", "man", "man1"))
}

func compareGeneratedTree(t *testing.T, generated, committed string) {
	t.Helper()
	generatedFiles := readGeneratedTree(t, generated)
	committedFiles := readGeneratedTree(t, committed)
	generatedPaths := sortedPaths(generatedFiles)
	committedPaths := sortedPaths(committedFiles)
	if !reflect.DeepEqual(generatedPaths, committedPaths) {
		t.Fatalf("generated documentation path drift in %s\ngenerated: %v\ncommitted: %v\nrun: go run -tags docsgen ./cmd/pmux", committed, generatedPaths, committedPaths)
	}
	for _, path := range generatedPaths {
		if !bytes.Equal(generatedFiles[path], committedFiles[path]) {
			t.Errorf("generated documentation content drift: %s; run: go run -tags docsgen ./cmd/pmux", filepath.Join(committed, filepath.FromSlash(path)))
		}
	}
}

func readGeneratedTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			t.Fatalf("generated documentation contains non-regular file: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = body
		return nil
	})
	if err != nil {
		t.Fatalf("read generated documentation tree %s: %v", root, err)
	}
	return files
}

func sortedPaths(files map[string][]byte) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func testModuleRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if body, readErr := os.ReadFile(filepath.Join(directory, "go.mod")); readErr == nil && bytes.Contains(body, []byte("module github.com/0p9b/pmux")) {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("could not find PMux module root from %s", directory)
		}
		directory = parent
	}
}
