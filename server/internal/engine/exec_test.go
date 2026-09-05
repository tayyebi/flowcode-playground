package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCappedBufferTruncates(t *testing.T) {
	b := newCappedBuffer(10)

	// A short write fits.
	n, err := b.Write([]byte("12345"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if b.Truncated() {
		t.Error("truncated too early")
	}

	// A write that straddles the limit keeps the prefix and flags the loss.
	n, err = b.Write([]byte("6789012345"))
	if err != nil {
		t.Fatalf("Write returned %v; a short count would make the child see an I/O error", err)
	}
	if n != 10 {
		t.Errorf("Write returned n=%d, want the full input length 10", n)
	}
	if got := b.String(); got != "1234567890" {
		t.Errorf("String = %q, want %q", got, "1234567890")
	}
	if !b.Truncated() {
		t.Error("Truncated = false after overflowing")
	}

	// Writes past the limit are absorbed silently.
	if _, err := b.Write([]byte("more")); err != nil {
		t.Errorf("Write past limit returned %v, want nil", err)
	}
	if len(b.String()) != 10 {
		t.Errorf("buffer grew past its limit to %d bytes", len(b.String()))
	}
}

func TestRunStageCapturesExitCodeAndStderr(t *testing.T) {
	res := runStage(context.Background(), 5*time.Second, 4096, t.TempDir(),
		"/bin/sh", "-c", "echo out; echo err >&2; exit 3")

	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "out") {
		t.Errorf("Stdout = %q, want it to contain %q", res.Stdout, "out")
	}
	if !strings.Contains(res.Stderr, "err") {
		t.Errorf("Stderr = %q, want it to contain %q", res.Stderr, "err")
	}
	if res.TimedOut {
		t.Error("TimedOut = true for a process that exited on its own")
	}
}

// TestRunStageKillsRunaway is the check that a program with an unbounded loop
// cannot hang the request. flowcode's VM has no fuel counter, so this path is
// load-bearing rather than theoretical.
func TestRunStageKillsRunaway(t *testing.T) {
	start := time.Now()
	res := runStage(context.Background(), 300*time.Millisecond, 4096, t.TempDir(),
		"/bin/sh", "-c", "while :; do :; done")
	elapsed := time.Since(start)

	if !res.TimedOut {
		t.Error("TimedOut = false for a process that never exits")
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %s to kill a runaway; the deadline is not being enforced", elapsed)
	}
}

// TestRunStageKillsProcessGroup covers the reason for Setpgid: killing only the
// leader would leave the child sleeping and hold the pipe open.
func TestRunStageKillsProcessGroup(t *testing.T) {
	start := time.Now()
	res := runStage(context.Background(), 300*time.Millisecond, 4096, t.TempDir(),
		"/bin/sh", "-c", "sleep 30 & wait")
	elapsed := time.Since(start)

	if !res.TimedOut {
		t.Error("TimedOut = false")
	}
	// WaitDelay is 2s; if the group was killed properly this returns well
	// inside that, rather than blocking until the grandchild's own exit.
	if elapsed > 5*time.Second {
		t.Errorf("took %s; a descendant appears to have outlived the kill", elapsed)
	}
}

func TestRunStageMissingBinary(t *testing.T) {
	res := runStage(context.Background(), time.Second, 4096, t.TempDir(),
		"/nonexistent/definitely-not-here")

	if res.Error == "" {
		t.Error("Error is empty; a binary that cannot be started must be distinguishable from a non-zero exit")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

func TestRunStageUsesCleanEnvironment(t *testing.T) {
	// The server's own environment may hold secrets; none of it should be
	// visible to a process running user-supplied input.
	t.Setenv("PLAYGROUND_SECRET_CANARY", "leaked")

	res := runStage(context.Background(), 5*time.Second, 4096, t.TempDir(),
		"/bin/sh", "-c", "echo \"[${PLAYGROUND_SECRET_CANARY:-unset}]\"")

	if strings.Contains(res.Stdout, "leaked") {
		t.Errorf("Stdout = %q; the parent environment leaked into the child", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "unset") {
		t.Errorf("Stdout = %q, want the canary to be unset", res.Stdout)
	}
}

func TestRunStageCapsOutput(t *testing.T) {
	res := runStage(context.Background(), 10*time.Second, 1024, t.TempDir(),
		"/bin/sh", "-c", "i=0; while [ $i -lt 5000 ]; do echo aaaaaaaaaaaaaaaaaaaa; i=$((i+1)); done")

	if !res.Truncated {
		t.Error("Truncated = false for output well past the limit")
	}
	if len(res.Stdout) > 1024 {
		t.Errorf("captured %d bytes, want at most 1024", len(res.Stdout))
	}
}
