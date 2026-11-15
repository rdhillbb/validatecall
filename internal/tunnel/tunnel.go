// Package tunnel runs the program that makes the events listener reachable
// from outside and learns the public address that program prints (R-26,
// TC-32). The address of a quick tunnel changes at every start, so it cannot
// be a setting.
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"time"
)

// addressWait is how long Start waits for the program to print its address.
const addressWait = 30 * time.Second

// address is the public address of a quick tunnel as the program prints it
// somewhere in a line of its output.
var address = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

var (
	errExited    = errors.New("exited before printing its address")
	errNoAddress = errors.New("printed no address")
)

// Tunnel is a running tunnel program.
type Tunnel struct {
	url  string
	cmd  *exec.Cmd
	done chan struct{}
}

// Start runs program with args and returns once a line of its output holds
// the tunnel's address. A program that exits first, or prints no address in
// time, is an error, and so is ctx ending during the wait; in each case the
// program has ended when Start returns. After Start has returned, ctx no
// longer matters: the program runs until Stop or until it exits by itself.
func Start(ctx context.Context, program string, args []string) (*Tunnel, error) {
	return start(ctx, program, args, addressWait)
}

func start(ctx context.Context, program string, args []string, wait time.Duration) (*Tunnel, error) {
	out, in, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("tunnel: open output pipe for %s: %w", program, err)
	}
	cmd := exec.Command(program, args...)
	// One pipe takes both streams: the address may come on either.
	cmd.Stdout, cmd.Stderr = in, in
	err = cmd.Start()
	// The program holds its own copy of the write end. Ours is closed so the
	// reader sees the end of the output when the program exits.
	in.Close()
	if err != nil {
		out.Close()
		return nil, fmt.Errorf("tunnel: start %s: %w", program, err)
	}

	t := &Tunnel{cmd: cmd, done: make(chan struct{})}
	found := make(chan string, 1)
	go read(out, found)
	go func() {
		// Wait's error only repeats the exit status, which ProcessState keeps.
		_ = cmd.Wait()
		// Releases the reader even if something else still holds the pipe.
		out.Close()
		close(t.done)
	}()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case t.url = <-found:
		return t, nil
	case <-t.done:
		return nil, fmt.Errorf("tunnel: %s: %w: %s", program, errExited, cmd.ProcessState)
	case <-timer.C:
		t.Stop()
		return nil, fmt.Errorf("tunnel: %s: %w within %s", program, errNoAddress, wait)
	case <-ctx.Done():
		t.Stop()
		return nil, fmt.Errorf("tunnel: %s: stopped while waiting for its address: %w", program, ctx.Err())
	}
}

// read sends the first address in the program's output to found, then keeps
// reading so the program never blocks on a full pipe.
func read(out io.Reader, found chan<- string) {
	// Scanner stops at a long log line and would discard every later address.
	lines := bufio.NewReader(out)
	for {
		line, err := lines.ReadString('\n')
		if url := address.FindString(line); url != "" {
			found <- url
			break
		}
		if err != nil {
			// EOF or the pipe being closed by Wait ends output; Start reports
			// the process exit, cancellation, or address timeout.
			return
		}
	}
	// Reading ends when the program exits and the pipe is closed; that end is
	// reported through Done, not here.
	_, _ = io.Copy(io.Discard, out)
}

// URL is the tunnel's public address, without a path.
func (t *Tunnel) URL() string {
	return t.url
}

// Done is closed when the program has exited, by Stop or by itself.
func (t *Tunnel) Done() <-chan struct{} {
	return t.done
}

// Stop ends the program and returns once it has exited. It may be called
// again, and after the program exited by itself.
func (t *Tunnel) Stop() {
	// Kill fails only when the program has already exited.
	_ = t.cmd.Process.Kill()
	<-t.done
}
