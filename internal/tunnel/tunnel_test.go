package tunnel

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The real tunnel program is never run: each test writes a shell script into
// its own temp folder and runs that instead.
const (
	fakeAddress = "https://fake-test-tunnel.trycloudflare.com"

	// scriptHead makes the script write its process id to its first argument.
	scriptHead = "#!/bin/sh\necho $$ > \"$1\"\n"
	// slowHead makes the script wait, before it writes its process id, until
	// the test opens the named pipe that is its second argument. The shell
	// itself waits, so ending it leaves no child of the script behind.
	slowHead = "#!/bin/sh\n: < \"$2\"\necho $$ > \"$1\"\n"
	// stayAlive replaces the shell with sleep, so the process id stays the
	// same and ending the program leaves no child of the script behind.
	stayAlive = "exec sleep 30\n"

	// printsAddress prints the address on the error stream, between other
	// lines, one of which names the tunnel service without being an address.
	printsAddress = "echo 'INF Requesting new quick Tunnel on trycloudflare.com...'\n" +
		"echo 'INF Your quick Tunnel has been created! Visit it at:' >&2\n" +
		"echo 'INF |  " + fakeAddress + "  |' >&2\n" +
		"echo 'INF Registered tunnel connection' >&2\n"

	// exitWait is far longer than a script needs to exit; shortWait is the
	// shortened wait of DD-2, long enough for the script to write its id.
	exitWait  = 20 * time.Second
	shortWait = time.Second
	// quickWait is a start wait that ends while a slowHead script still waits.
	quickWait = 100 * time.Millisecond
	// doneWait bounds how long a test waits for Done after the program ended.
	doneWait = 10 * time.Second
	// Exceeds Scanner's default token limit; tunnel output has no line-size contract.
	longOutputSize = 128 * 1024
)

// fakeProgram writes a script with body into the test's temp folder. It
// returns the script's path and the arguments to start it with: the file the
// script writes its process id to. When the test ends, a script that still
// runs is ended and reported, so no test leaves a process behind.
func fakeProgram(t *testing.T, body string) (program string, args []string) {
	t.Helper()
	return fakeScript(t, scriptHead+body)
}

// slowFakeProgram is a fakeProgram that stays alive but writes its process id
// only after the test opens the pipe named by the last of args.
func slowFakeProgram(t *testing.T) (program string, args []string) {
	t.Helper()
	program, args = fakeScript(t, slowHead+stayAlive)
	pipe := filepath.Join(t.TempDir(), "go-on")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatalf("make the fake program's pipe: %v", err)
	}
	return program, append(args, pipe)
}

// fakeScript writes script into the test's temp folder; see fakeProgram.
func fakeScript(t *testing.T, script string) (program string, args []string) {
	t.Helper()
	dir := t.TempDir()
	program = filepath.Join(dir, "fake-tunnel")
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake program: %v", err)
	}
	pidFile := filepath.Join(dir, "pid")
	t.Cleanup(func() {
		pid, running := leftRunning(t, []string{pidFile})
		if !running {
			return
		}
		// Wait4 answers 0 only for a child of this test process that still
		// runs, so the signal cannot reach any other process.
		if got, _ := syscall.Wait4(pid, nil, syscall.WNOHANG, nil); got == 0 {
			t.Errorf("fake program %d was still running when the test ended", pid)
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				t.Errorf("end fake program %d: %v", pid, err)
			}
		}
	})
	return program, []string{pidFile}
}

// programID reads the process id the script wrote.
func programID(t *testing.T, args []string) int {
	t.Helper()
	pid, written := readID(t, args)
	if !written {
		t.Fatalf("the fake program did not write its complete process id to %s", args[0])
	}
	return pid
}

// readID reads the process id the script wrote, and reports false while the
// script has not written all of it yet.
func readID(t *testing.T, args []string) (pid int, written bool) {
	t.Helper()
	raw, err := os.ReadFile(args[0])
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read the fake program's process id: %v", err)
	}
	// echo ends the id with a newline; until then the file is empty or partial.
	if !strings.HasSuffix(string(raw), "\n") {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("fake program's process id %q: %v", raw, err)
	}
	return pid, true
}

func leftRunning(t *testing.T, args []string) (pid int, running bool) {
	t.Helper()
	if pid, written := readID(t, args); written {
		return pid, exists(pid)
	}

	// These tests run serially with at most one fake tunnel child at a time.
	// Its parent PID identifies it even before the shell can write its own ID.
	ctx, cancel := context.WithTimeout(context.Background(), exitWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("list processes to check the fake tunnel: %v", err)
	}
	columns := strings.Fields(string(out))
	if len(columns)%2 != 0 {
		t.Fatal("process listing did not contain PID and parent PID pairs")
	}
	parent := strconv.Itoa(os.Getpid())
	listing := strconv.Itoa(cmd.Process.Pid)
	for i := 0; i < len(columns); i += 2 {
		if columns[i+1] != parent || columns[i] == listing {
			continue
		}
		pid, err := strconv.Atoi(columns[i])
		if err != nil {
			t.Fatalf("read the fake tunnel's process id from ps: %v", err)
		}
		return pid, true
	}
	return 0, false
}

// exists reports whether process pid is still there. A program that was
// ended but not waited for still counts as there.
func exists(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func isDone(tun *Tunnel) bool {
	select {
	case <-tun.Done():
		return true
	default:
		return false
	}
}

func TestStartFindsAddress(t *testing.T) {
	longLine := strings.Repeat("x", longOutputSize)
	for _, tc := range []struct{ name, output string }{
		{"ordinary output", printsAddress},
		{"long preceding line", "echo '" + longLine + "'\n" + printsAddress},
		{"address in long line", "echo '" + longLine + " " + fakeAddress + "'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			program, args := fakeProgram(t, tc.output+stayAlive)
			tun, err := start(context.Background(), program, args, shortWait)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(tun.Stop)

			if got := tun.URL(); got != fakeAddress {
				t.Errorf("URL = %q, want %q", got, fakeAddress)
			}
			if isDone(tun) {
				t.Errorf("Done is closed while the program still runs")
			}
			if pid := programID(t, args); !exists(pid) {
				t.Errorf("program %d is gone after Start returned its address", pid)
			}
		})
	}
}

func TestStopEndsProgram(t *testing.T) {
	program, args := fakeProgram(t, printsAddress+stayAlive)

	tun, err := Start(context.Background(), program, args)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(tun.Stop)
	pid := programID(t, args)
	if !exists(pid) || isDone(tun) {
		t.Fatalf("before Stop: program %d exists = %t, Done closed = %t; want a running program", pid, exists(pid), isDone(tun))
	}

	tun.Stop()

	if !isDone(tun) {
		t.Errorf("Done is not closed after Stop")
	}
	if exists(pid) {
		t.Errorf("program %d still exists after Stop", pid)
	}
	// A second Stop must return too: main stops the tunnel at exit even
	// when the tunnel's own exit is why it stops.
	tun.Stop()
}

func TestDoneClosesWhenProgramExits(t *testing.T) {
	program, args := fakeProgram(t, printsAddress+stayAlive)

	tun, err := Start(context.Background(), program, args)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(tun.Stop)
	pid := programID(t, args)
	if isDone(tun) {
		t.Fatalf("Done is closed while program %d still runs", pid)
	}

	// The program ends without Stop, as a tunnel that fails would.
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("end program %d: %v", pid, err)
	}

	select {
	case <-tun.Done():
	case <-time.After(doneWait):
		t.Fatalf("Done not closed %s after the program was ended", doneWait)
	}
	if exists(pid) {
		t.Errorf("program %d still exists after Done closed", pid)
	}
}

func TestStartFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		wait time.Duration
		// stopAfter, when set, ends the caller's context after that long.
		stopAfter time.Duration
		want      error
		wantText  string
	}{
		{
			name:     "exits with a failure after other output",
			body:     "echo 'ERR failed to start the tunnel'\nexit 3\n",
			wait:     exitWait,
			want:     errExited,
			wantText: "exit status 3",
		},
		{
			name:     "exits without a failure and without an address",
			body:     "echo 'INF nothing to do'\n",
			wait:     exitWait,
			want:     errExited,
			wantText: "exit status 0",
		},
		{
			name:     "prints no address before the wait ends",
			body:     "echo 'INF Requesting new quick Tunnel on trycloudflare.com...'\n" + stayAlive,
			wait:     shortWait,
			want:     errNoAddress,
			wantText: shortWait.String(),
		},
		{
			name:      "caller stops during the wait",
			body:      "echo 'INF Requesting new quick Tunnel on trycloudflare.com...'\n" + stayAlive,
			wait:      exitWait,
			stopAfter: shortWait,
			want:      context.DeadlineExceeded,
			wantText:  "stopped while waiting",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			program, args := fakeProgram(t, tc.body)
			ctx := context.Background()
			if tc.stopAfter > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.stopAfter)
				defer cancel()
			}

			tun, err := start(ctx, program, args, tc.wait)
			if tun != nil {
				t.Cleanup(tun.Stop)
			}

			if err == nil || tun != nil {
				t.Fatalf("start = %v, %v; want no tunnel and an error", tun, err)
			}
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("start error = %q, want %q with %q", err, tc.want, tc.wantText)
			}
			if !strings.Contains(err.Error(), program) {
				t.Errorf("start error %q does not name the program %s", err, program)
			}
			if pid, running := leftRunning(t, args); running {
				t.Errorf("program %d still exists after start failed", pid)
			}
		})
	}
}

// On a busy machine the start wait can end before the script has written its
// process id; the check of TestStartFails must still hold.
func TestStartFailsBeforeProgramID(t *testing.T) {
	t.Run("ended before it wrote its id", func(t *testing.T) {
		program, args := slowFakeProgram(t)

		tun, err := start(context.Background(), program, args, quickWait)
		if tun != nil {
			t.Cleanup(tun.Stop)
		}

		if tun != nil || !errors.Is(err, errNoAddress) {
			t.Fatalf("start = %v, %v; want no tunnel and %q", tun, err, errNoAddress)
		}
		if pid, running := leftRunning(t, args); running {
			t.Errorf("program %d still exists after start failed", pid)
		}
		if _, written := readID(t, args); written {
			t.Errorf("the program wrote its process id; want it ended before that")
		}
	})

	t.Run("left running", func(t *testing.T) {
		program, args := slowFakeProgram(t)
		// Keep the FIFO closed to model a leaked child which cannot publish its PID.
		cmd := exec.Command(program, args...)
		if err := cmd.Start(); err != nil {
			t.Fatalf("run the fake program: %v", err)
		}
		t.Cleanup(func() {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("end the fake program: %v", err)
				return
			}
			var exitErr *exec.ExitError
			if err := cmd.Wait(); err != nil && !errors.As(err, &exitErr) {
				t.Errorf("reap the fake program: %v", err)
			}
		})
		if _, written := readID(t, args); written {
			t.Fatal("the program wrote its process id before the FIFO opened")
		}
		if pid, running := leftRunning(t, args); !running || pid != cmd.Process.Pid {
			t.Errorf("leftRunning before PID file = %d, %t; want live program %d", pid, running, cmd.Process.Pid)
		}

		// Exercise the recorded-PID path with the same child and no scheduler handshake.
		if err := os.WriteFile(args[0], []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
			t.Fatalf("write the live program's process id: %v", err)
		}
		if pid, running := leftRunning(t, args); !running || pid != cmd.Process.Pid {
			t.Errorf("leftRunning with PID file = %d, %t; want live program %d", pid, running, cmd.Process.Pid)
		}
	})
}

func TestStartFailsWithoutProgram(t *testing.T) {
	program := filepath.Join(t.TempDir(), "no-such-tunnel-program")

	tun, err := Start(context.Background(), program, nil)
	if tun != nil {
		t.Cleanup(tun.Stop)
	}

	if tun != nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Start of a missing program = %v, %v; want no tunnel and a not-found error", tun, err)
	}
	if !strings.Contains(err.Error(), program) {
		t.Errorf("Start error %q does not name the program %s", err, program)
	}
}
