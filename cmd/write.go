package cmd

import (
	"fmt"
	"os"
	"path/filepath"
)

// privateFileMode is the permission of the files clrnd generates. render's output can contain
// secrets expanded with must_env, and init's output can contain the live service's plaintext
// env[].value, so keep them unreadable by other users.
const privateFileMode os.FileMode = 0o600

// writeFilePrivate writes data to path and always leaves the finished file with privateFileMode
// permissions.
//
// os.WriteFile is not enough. Its perm is applied only when the file is created, so an existing
// 0644 destination stays 0644 even after a secret is written into it. And an existing file is
// truncated before writing, so a failure part-way through loses the previous good content as well.
// Writing the whole content to a temporary file in the same directory and then renaming it avoids
// each of these.
//
// "The replacement is atomic, so path always holds either the old or the new content" holds only
// for the Unix rename. Windows' os.Rename is MoveFileEx(MOVEFILE_REPLACE_EXISTING), whose
// replacement the OS does not guarantee to be atomic, so an intermediate state after a crash is
// possible. Going through a temporary file still has a point there: a failed write (disk full /
// I/O error) never touches the destination, so the previous content remains and a partially
// written file is never exposed.
// The mode follows the same line: Windows reduces file permissions to the read-only bit, so 0600
// means "unreadable by other users" only on Unix.
//
// On Windows the rename fails when another process has the destination open, but that is only
// returned as a failure; the destination's content is not damaged.
func writeFilePrivate(path string, data []byte) error {
	// A destination that is not a regular file, such as /dev/null or a process substitution
	// (>(cmd)), has no content to replace and no permissions to set. It cannot be renamed over
	// either, so write to it directly.
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		if err := os.WriteFile(path, data, privateFileMode); err != nil {
			return fmt.Errorf("failed to write to %s: %w", path, err)
		}
		return nil
	}

	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return fmt.Errorf("failed to write to %s: %w", path, err)
	}
	tmp := f.Name()
	// Do not leave the temporary file behind on any failure path. tmp is cleared once the rename
	// succeeds.
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()

	if err := writeAndClose(f, data); err != nil {
		return fmt.Errorf("failed to write to %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("failed to write to %s: %w", path, err)
	}
	tmp = ""
	return nil
}

// writeAndClose writes the whole content to an open file and closes it. The perm given at creation
// is reduced by the umask, so the permissions are set again explicitly to pin them down.
func writeAndClose(f *os.File, data []byte) error {
	defer func() { _ = f.Close() }()

	if err := f.Chmod(privateFileMode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	// Renaming before the written content reaches the disk can leave an empty file after a crash.
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// writeFileExclusive creates and writes path only when it does not exist yet. The existence check
// and the creation happen in a single O_CREATE|O_EXCL open, so a file created after the check but
// before the write is not overwritten (preventing an existing file from being lost without
// --force). If the write fails, it removes the file it created and leaves nothing behind.
func writeFileExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists: pass --force to overwrite", path)
		}
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	if err := writeAndClose(f, data); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}
