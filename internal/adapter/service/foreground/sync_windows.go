//go:build windows

package foreground

// Windows rename durability is provided by the platform rename primitive; Go
// cannot open directory handles with os.File for fsync.
func syncDirectory(string) error { return nil }
