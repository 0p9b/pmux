package runtime

import (
	"path/filepath"

	domainplatform "github.com/0p9b/pmux/internal/domain/platform"
)

func testRoots(root string) domainplatform.Roots {
	return domainplatform.Roots{
		Config: filepath.Join(root, "config"),
		State:  filepath.Join(root, "state"),
		Cache:  filepath.Join(root, "cache"),
		Data:   filepath.Join(root, "data"),
	}
}

func normalizeTestPath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return path
}
