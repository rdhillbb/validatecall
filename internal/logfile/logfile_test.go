package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Small values so no test writes 2 MB (TC-46).
const (
	testLimit = 20
	testKeep  = 5
)

// openTest opens path with the small test limit and returns the file and the
// buffer that receives its warnings.
func openTest(t *testing.T, path string) (*File, *bytes.Buffer) {
	t.Helper()
	warn := &bytes.Buffer{}
	f, err := open(path, testLimit, testKeep, warn)
	if err != nil {
		t.Fatalf("open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, warn
}

func mustWrite(t *testing.T, f *File, line string) {
	t.Helper()
	n, err := f.Write([]byte(line))
	if err != nil {
		t.Fatalf("Write(%q): %v", line, err)
	}
	if n != len(line) {
		t.Fatalf("Write(%q) = %d bytes; want %d", line, n, len(line))
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s: stat error = %v; want the file not to exist", path, err)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	if got := readFile(t, path); got != want {
		t.Errorf("%s holds %q; want %q", path, got, want)
	}
}

func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o; want %o", path, got, want)
	}
}

func setAsidePath(path string, n int) string {
	return fmt.Sprintf("%s.%d", path, n)
}

func TestSetAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	f, warn := openTest(t, path)

	first, second, third := "aaaaaaaaa\n", "bbbbbbbbb\n", "ccc\n"
	mustWrite(t, f, first)
	mustWrite(t, f, second) // exactly the limit: no set-aside yet
	assertContent(t, path, first+second)
	assertMissing(t, setAsidePath(path, 1))

	mustWrite(t, f, third) // 20 + 4 would pass the limit
	assertContent(t, setAsidePath(path, 1), first+second)
	assertContent(t, path, third)
	assertMissing(t, setAsidePath(path, 2))

	for _, p := range []string{path, setAsidePath(path, 1)} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if info.Size() > testLimit {
			t.Errorf("%s size = %d; want at most %d", p, info.Size(), testLimit)
		}
	}
	if warn.Len() != 0 {
		t.Errorf("warnings = %q; want none", warn.String())
	}
}

func TestLineLargerThanLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	f, warn := openTest(t, path)
	big := strings.Repeat("x", 2*testLimit) + "\n"

	// Into an empty file the large line goes at once, without setting aside.
	mustWrite(t, f, big)
	assertContent(t, path, big)
	assertMissing(t, setAsidePath(path, 1))

	// The next line sets the large one aside once; no empty file is made.
	mustWrite(t, f, "small\n")
	assertContent(t, setAsidePath(path, 1), big)
	assertContent(t, path, "small\n")
	assertMissing(t, setAsidePath(path, 2))

	// A large line after a small one: the small one is set aside, then the
	// large one goes into the new, empty file exactly once.
	mustWrite(t, f, big)
	assertContent(t, setAsidePath(path, 1), "small\n")
	assertContent(t, setAsidePath(path, 2), big)
	assertContent(t, path, big)
	assertMissing(t, setAsidePath(path, 3))
	if warn.Len() != 0 {
		t.Errorf("warnings = %q; want none", warn.String())
	}
}

func TestKeepFive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	f, warn := openTest(t, path)

	// Each line is 12 bytes; two never fit under the 20-byte limit, so
	// every write after the first sets the file aside: 7 set-asides.
	const lines = 8
	for i := 1; i <= lines; i++ {
		mustWrite(t, f, fmt.Sprintf("line-%02d....\n", i))
	}

	assertContent(t, path, "line-08....\n")
	for n := 1; n <= testKeep; n++ {
		assertContent(t, setAsidePath(path, n), fmt.Sprintf("line-%02d....\n", lines-n))
	}
	assertMissing(t, setAsidePath(path, testKeep+1))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != testKeep+1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("folder holds %v; want the log and %d set-aside files", names, testKeep)
	}
	for _, e := range entries {
		content := readFile(t, filepath.Join(dir, e.Name()))
		if strings.HasPrefix(content, "line-01") || strings.HasPrefix(content, "line-02") {
			t.Errorf("%s still holds the oldest content %q", e.Name(), content)
		}
	}
	if warn.Len() != 0 {
		t.Errorf("warnings = %q; want none", warn.String())
	}
}

func TestGapsSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	seed := map[string]string{
		path:                  "current..........\n",
		setAsidePath(path, 1): "old-one\n",
		setAsidePath(path, 3): "old-three\n",
	}
	for p, content := range seed {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	f, warn := openTest(t, path)
	mustWrite(t, f, "new\n") // 18 + 4 would pass the limit

	assertContent(t, path, "new\n")
	assertContent(t, setAsidePath(path, 1), "current..........\n")
	assertContent(t, setAsidePath(path, 2), "old-one\n")
	assertMissing(t, setAsidePath(path, 3))
	assertContent(t, setAsidePath(path, 4), "old-three\n")
	assertMissing(t, setAsidePath(path, 5))
	if warn.Len() != 0 {
		t.Errorf("warnings = %q; want none", warn.String())
	}
}

// writeLines writes lines to a log at path, closing and reopening the file
// before line index restartAt (no restart when restartAt < 0). It returns the
// index of the first write that set the file aside and the final contents of
// the log and its first set-aside file.
func writeLines(t *testing.T, path string, lines []string, restartAt int) (int, string, string) {
	t.Helper()
	f, err := open(path, testLimit, testKeep, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("open(%s): %v", path, err)
	}
	firstSetAside := -1
	for i, line := range lines {
		if i == restartAt {
			if err := f.Close(); err != nil {
				t.Fatalf("close before restart: %v", err)
			}
			if f, err = open(path, testLimit, testKeep, &bytes.Buffer{}); err != nil {
				t.Fatalf("reopen(%s): %v", path, err)
			}
		}
		mustWrite(t, f, line)
		if _, err := os.Stat(setAsidePath(path, 1)); err == nil && firstSetAside < 0 {
			firstSetAside = i
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return firstSetAside, readFile(t, path), readFile(t, setAsidePath(path, 1))
}

func TestRestartKeepsCount(t *testing.T) {
	lines := []string{"one..\n", "two..\n", "three\n", "four.\n", "five.\n"}

	wantIndex, wantLog, wantAside := writeLines(t, filepath.Join(t.TempDir(), "app.log"), lines, -1)
	if wantIndex != 3 {
		t.Fatalf("without restart the first set-aside came at write %d; want 3 (three 6-byte lines fill 18 of 20 bytes)", wantIndex)
	}

	gotIndex, gotLog, gotAside := writeLines(t, filepath.Join(t.TempDir(), "app.log"), lines, 2)
	if gotIndex != wantIndex {
		t.Errorf("with a restart the first set-aside came at write %d; want %d as without", gotIndex, wantIndex)
	}
	if gotLog != wantLog || gotAside != wantAside {
		t.Errorf("with a restart log = %q, .1 = %q; want %q, %q as without", gotLog, gotAside, wantLog, wantAside)
	}
}

func TestSetAsideFailureKeepsWriting(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores folder permissions, so a read-only folder cannot make setting aside fail")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	f, warn := openTest(t, path)

	const first, second = "first-line.....\n", "secret-content\n"
	mustWrite(t, f, first)

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("make folder read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	n, err := f.Write([]byte(second)) // 16 + 15 would pass the limit
	if err != nil {
		t.Fatalf("Write returned %v; want no error when setting aside fails", err)
	}
	if n != len(second) {
		t.Errorf("Write = %d bytes; want %d", n, len(second))
	}

	assertContent(t, path, first+second)
	assertMissing(t, setAsidePath(path, 1))

	got := warn.String()
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Errorf("warnings = %q; want exactly one line", got)
	}
	if !strings.Contains(got, path) {
		t.Errorf("warning %q does not name the path %s", got, path)
	}
	if strings.Contains(got, "secret-content") || strings.Contains(got, "first-line") {
		t.Errorf("warning %q holds line content", got)
	}
}

func TestWriteAfterCloseSetsNothingAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	f, warn := openTest(t, path)
	const first = "first-line.....\n"
	mustWrite(t, f, first)
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	n, err := f.Write([]byte("after-close....\n")) // 16 + 16 would pass the limit
	if !errors.Is(err, os.ErrClosed) || n != 0 {
		t.Errorf("Write after Close = %d, %v; want 0, os.ErrClosed", n, err)
	}
	assertContent(t, path, first)
	assertMissing(t, setAsidePath(path, 1))
	if warn.Len() != 0 {
		t.Errorf("warnings = %q; want none", warn.String())
	}
}

// TestModes changes the process umask, so it must not run in parallel.
func TestModes(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })

	t.Run("new folder and file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "logs")
		dir := filepath.Join(parent, "checknumber")
		path := filepath.Join(dir, "app.log")
		f, err := Open(path)
		if err != nil {
			t.Fatalf("Open(%s): %v", path, err)
		}
		t.Cleanup(func() { _ = f.Close() })
		assertMode(t, parent, 0o755)
		assertMode(t, dir, 0o755)
		assertMode(t, path, 0o644)
	})

	t.Run("new file after setting aside", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.log")
		f, _ := openTest(t, path)
		mustWrite(t, f, "aaaaaaaaaaaaaaa\n")
		mustWrite(t, f, "bbbbbbbbbbbbbbb\n")
		assertContent(t, path, "bbbbbbbbbbbbbbb\n")
		assertMode(t, path, 0o644)
	})

	t.Run("existing folder and file unchanged", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "kept")
		path := filepath.Join(dir, "app.log")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Explicit modes that differ from 755 and 644, independent of the umask.
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatalf("chmod dir: %v", err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatalf("chmod file: %v", err)
		}
		f, err := Open(path)
		if err != nil {
			t.Fatalf("Open(%s): %v", path, err)
		}
		t.Cleanup(func() { _ = f.Close() })
		mustWrite(t, f, "new\n")
		assertContent(t, path, "old\nnew\n")
		assertMode(t, dir, 0o750)
		assertMode(t, path, 0o640)
	})
}
