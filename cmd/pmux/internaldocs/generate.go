package internaldocs

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

var manualDate = time.Date(2026, time.July, 20, 0, 0, 0, 0, time.UTC)

// Generate replaces only the two explicitly supplied generated trees.
func Generate(newRoot func() *cobra.Command, referenceDir, manDir string) error {
	if newRoot == nil {
		return fmt.Errorf("nil Cobra root factory")
	}
	for _, directory := range []string{referenceDir, manDir} {
		if directory == "" || filepath.Clean(directory) == "." || filepath.Clean(directory) == string(filepath.Separator) {
			return fmt.Errorf("refusing unsafe generated-doc directory %q", directory)
		}
		if err := os.RemoveAll(directory); err != nil {
			return fmt.Errorf("remove generated-doc directory %s: %w", directory, err)
		}
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create generated-doc directory %s: %w", directory, err)
		}
	}
	referenceRoot := newRoot()
	if referenceRoot == nil {
		return fmt.Errorf("cobra root factory returned nil")
	}
	setDeterministic(referenceRoot)
	if err := doc.GenMarkdownTree(referenceRoot, referenceDir); err != nil {
		return fmt.Errorf("generate command reference: %w", err)
	}
	manRoot := newRoot()
	if manRoot == nil {
		return fmt.Errorf("cobra root factory returned nil")
	}
	setDeterministic(manRoot)
	header := &doc.GenManHeader{Title: "PMUX", Section: "1", Source: "PMux", Manual: "PMux Manual", Date: &manualDate}
	if err := doc.GenManTree(manRoot, header, manDir); err != nil {
		return fmt.Errorf("generate man pages: %w", err)
	}
	return nil
}

func setDeterministic(command *cobra.Command) {
	command.DisableAutoGenTag = true
	for _, child := range command.Commands() {
		setDeterministic(child)
	}
}
