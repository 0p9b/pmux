package fs

import (
	"errors"
	"io"
	"os"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

// TrimPartialLine removes only an unterminated final line. It is intended to
// run immediately before an append while the caller holds its mutation lock;
// readers may safely ignore the same torn tail without modifying the file.
func TrimPartialLine(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not open append-only file for crash recovery")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not inspect append-only file for crash recovery")
	}
	if info.Size() == 0 {
		return nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], info.Size()-1); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not inspect append-only tail")
	}
	if last[0] == '\n' {
		return nil
	}

	const blockSize int64 = 4096
	end := info.Size()
	truncateAt := int64(0)
	for end > 0 {
		start := end - blockSize
		if start < 0 {
			start = 0
		}
		block := make([]byte, end-start)
		if _, err := file.ReadAt(block, start); err != nil && !errors.Is(err, io.EOF) {
			return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not scan append-only tail")
		}
		for index := len(block) - 1; index >= 0; index-- {
			if block[index] == '\n' {
				truncateAt = start + int64(index) + 1
				end = 0
				break
			}
		}
		if truncateAt == 0 {
			end = start
		}
	}
	if err := file.Truncate(truncateAt); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not remove torn append-only record")
	}
	if err := file.Sync(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush append-only crash recovery")
	}
	return nil
}
