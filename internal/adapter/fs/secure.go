package fs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

// EnsurePrivateDir creates a directory tree and enforces owner-only access.
func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not create private directory")
	}
	return protectPrivatePath(path, true)
}

// AtomicWritePrivate durably replaces path with owner-only payload bytes. The
// temporary is created in the target directory, fsynced, renamed, then the
// directory itself is fsynced. The original remains intact before rename.
func AtomicWritePrivate(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not create target directory")
	}
	tmp, err := os.CreateTemp(dir, ".pmux-write-*")
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not create private temporary file")
	}
	name := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(name)
		}
	}()
	if err := protectPrivatePath(name, false); err != nil {
		return err
	}
	if _, err := io.Copy(tmp, bytes.NewReader(payload)); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not write private temporary file")
	}
	if err := tmp.Sync(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush private temporary file")
	}
	if err := tmp.Close(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not close private temporary file")
	}
	if err := replaceFile(name, path); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "could not atomically replace private file")
	}
	if err := protectPrivatePath(path, false); err != nil {
		return err
	}
	if err := SyncDirectory(dir); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush private directory")
	}
	committed = true
	return nil
}

// WritePrivateExclusive durably creates path and refuses to overwrite it.
func WritePrivateExclusive(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := EnsurePrivateDir(dir); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "private file already exists; refusing to overwrite it")
		}
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not create private file")
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if err := protectPrivatePath(path, false); err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not write private file")
	}
	if err := file.Sync(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush private file")
	}
	if err := file.Close(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not close private file")
	}
	if err := protectPrivatePath(path, false); err != nil {
		return err
	}
	if err := SyncDirectory(dir); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush private directory")
	}
	complete = true
	return nil
}

// AppendPrivate appends one record, fsyncs it, and fsyncs the parent when the
// file is first created. One call performs one Write to reduce torn records.
func AppendPrivate(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := EnsurePrivateDir(dir); err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not open private append-only file")
	}
	if err := protectPrivatePath(path, false); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not append private record")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush private append-only file")
	}
	if err := file.Close(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not close private append-only file")
	}
	if created {
		if err := SyncDirectory(dir); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush private append-only directory")
		}
	}
	return nil
}

func SyncDirectory(path string) error {
	return syncDirectoryPlatform(path)
}
