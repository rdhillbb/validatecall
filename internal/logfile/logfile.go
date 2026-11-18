// Package logfile is the app's log file: before a write would take it past
// MaxBytes it is set aside as path.1, older ones move up, and only Keep
// set-aside files are kept (R-30, TC-46). Standard library only (TC-49).
package logfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

const (
	// MaxBytes is the size the log never passes, except for one line that is
	// itself larger (operator 2026-11-17: "2 MB").
	MaxBytes = 2 << 20
	// Keep is the number of set-aside files kept (operator 2026-11-17: "5").
	Keep = 5

	// Rights as today's log (C-8, TC-47), set explicitly so the umask
	// cannot narrow them.
	fileMode fs.FileMode = 0o644
	dirMode  fs.FileMode = 0o755
)

// File is a log file that sets itself aside. It is safe for use from many
// goroutines.
type File struct {
	mu     sync.Mutex
	path   string
	limit  int64
	keep   int
	warn   io.Writer
	file   *os.File
	size   int64
	closed bool
}

// Open opens or creates the log at path for appending, creating a missing
// folder, and continues the size count of an existing file.
func Open(path string) (*File, error) {
	return open(path, MaxBytes, Keep, os.Stderr)
}

// open is Open with the limit, the count and the warning writer chosen by the
// caller, so tests need not write 2 MB.
func open(path string, limit int64, keep int, warn io.Writer) (*File, error) {
	if err := makeDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("logfile: open %s: %w", path, err)
	}
	file, err := openLog(path)
	if err != nil {
		return nil, fmt.Errorf("logfile: open %s: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("logfile: open %s: %w", path, err)
	}
	return &File{path: path, limit: limit, keep: keep, warn: warn, file: file, size: info.Size()}, nil
}

// Write appends p, first setting the file aside when it is not empty and p
// would take it past the limit. A failure to set aside is reported on the
// warning writer and p still goes into the current file: a log fault must
// never cost a check (TC-46).
func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// After Close no set-aside may run: it would open a new log that nothing
	// ever closes.
	if f.closed {
		return 0, fmt.Errorf("logfile: write %s: %w", f.path, os.ErrClosed)
	}
	if f.size > 0 && f.size+int64(len(p)) > f.limit {
		if err := f.setAside(); err != nil {
			// Nothing better can be done when standard error itself fails.
			_, _ = fmt.Fprintf(f.warn, "logfile: set aside %s: %v\n", f.path, err)
		}
	}
	n, err := f.file.Write(p)
	f.size += int64(n)
	return n, err
}

// Close closes the current file.
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return f.file.Close()
}

// setAside deletes the oldest kept file, moves the others up by one (missing
// ones are skipped), renames the log to .1 and starts a new log. On an error
// the current file stays in use.
func (f *File) setAside() error {
	if err := os.Remove(f.numbered(f.keep)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for n := f.keep - 1; n >= 1; n-- {
		if err := os.Rename(f.numbered(n), f.numbered(n+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(f.path, f.numbered(1)); err != nil {
		return err
	}
	file, err := openLog(f.path)
	if err != nil {
		// Put the log back so the lines that follow still land at the path.
		return errors.Join(err, os.Rename(f.numbered(1), f.path))
	}
	if err := f.file.Close(); err != nil {
		_, _ = fmt.Fprintf(f.warn, "logfile: close set-aside %s: %v\n", f.numbered(1), err)
	}
	f.file = file
	f.size = 0
	return nil
}

func (f *File) numbered(n int) string {
	return fmt.Sprintf("%s.%d", f.path, n)
}

// openLog opens path for appending. Only a file it creates gets fileMode set;
// an existing file keeps its rights (C-8).
func openLog(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, fileMode)
	if errors.Is(err, fs.ErrExist) {
		return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	}
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(fileMode); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// makeDir creates dir and any missing parents with dirMode. Folders that
// already exist keep their rights (C-8).
func makeDir(dir string) error {
	_, err := os.Stat(dir)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := makeDir(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, dirMode); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return os.Chmod(dir, dirMode)
}
