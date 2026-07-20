//go:build docsgen

package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/0p9b/pmux/cmd/pmux/internaldocs"
	"github.com/0p9b/pmux/internal/app"
	"github.com/spf13/cobra"
)

func main() {
	outputRoot := flag.String("output-root", "", "repository root receiving generated documentation")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "docsgen accepts only --output-root")
		os.Exit(2)
	}
	root := *outputRoot
	if root == "" {
		var err error
		root, err = findModuleRoot()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	} else {
		absolute, err := filepath.Abs(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		root = absolute
	}
	err := internaldocs.Generate(func() *cobra.Command {
		return newRootCommand(dependencies{UseCases: app.UnavailableUseCases{}, IsTerminal: func() bool { return false }})
	}, filepath.Join(root, "docs", "reference", "commands"), filepath.Join(root, "docs", "man", "man1"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func findModuleRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil { return "", fmt.Errorf("read working directory: %w", err) }
	for {
		moduleFile := filepath.Join(directory, "go.mod")
		body, readErr := os.ReadFile(moduleFile)
		if readErr == nil && bytes.Contains(body, []byte("module github.com/0p9b/pmux")) { return directory, nil }
		parent := filepath.Dir(directory)
		if parent == directory { return "", fmt.Errorf("could not find github.com/0p9b/pmux module root from %s", directory) }
		directory = parent
	}
}
