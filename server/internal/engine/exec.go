package engine

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"
)

// cappedBuffer collects output up to a byte limit and then silently drops the
// rest, recording that it did. A playground program can emit output far faster
// than anyone can read it — the trace for a tight loop is unbounded — so the
// response size has to be capped somewhere, and capping at the source keeps the
// memory bounded too.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

// Write never returns an error or a short count: reporting one would make the
// child process see a failed write and possibly die with an I/O error, when
// what we actually want is for it to keep running and simply have its extra
// output discarded.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *cappedBuffer) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// StageResult is the outcome of running one binary (fcc or fcplay).
type StageResult struct {
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr"`
	TimedOut   bool   `json:"timedOut"`
	DurationMs int64  `json:"durationMs"`
	Truncated  bool   `json:"truncated"`
	// Error is set only when the process could not be run at all — a missing
	// binary, say. It is distinct from the program exiting non-zero.
	Error string `json:"error,omitempty"`
}

// runStage executes one sandboxed command.
//
// Isolation here is layered, and each layer covers a gap the others don't:
//
//   - the container runs unprivileged, read-only, with dropped capabilities;
//   - fcplay installs its own rlimits (CPU, address space, file size, nproc);
//   - the wall-clock timeout below catches anything rlimits can't, since
//     RLIMIT_CPU only counts CPU time and a blocked process burns none;
//   - output is capped so a chatty program can't exhaust memory.
//
// The child is put in its own process group so the timeout can kill the whole
// group. Signalling just the leader would leave any descendant running — the
// VM never forks today, but the cost of being right here is one syscall.
func runStage(ctx context.Context, timeout time.Duration, outputLimit int, dir, name string, args ...string) StageResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout := newCappedBuffer(outputLimit)
	stderr := newCappedBuffer(outputLimit)

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = nil
	// A deliberately bare environment: nothing from the server's own env leaks
	// into a process running user-supplied input.
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LC_ALL=C"}
	configureSandbox(cmd)

	// Kill the process group rather than the single process, and give it a
	// moment to die before we stop waiting on it.
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	res := StageResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMs: elapsed.Milliseconds(),
		Truncated:  stdout.Truncated() || stderr.Truncated(),
	}

	// ctx.Err() is the reliable signal: a killed process reports a signal exit,
	// not a distinguishable timeout, so we ask the context what happened.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		return res
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			// Couldn't start it at all: binary missing, not executable, ...
			res.ExitCode = -1
			res.Error = err.Error()
		}
		return res
	}

	res.ExitCode = 0
	return res
}
