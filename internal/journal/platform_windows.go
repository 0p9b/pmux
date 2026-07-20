//go:build windows

package journal

// The append itself is flushed with FlushFileBuffers through os.File.Sync.
// Windows has no POSIX directory fsync through os.File.
func syncJournalDirectory(string) error {
	return nil
}
