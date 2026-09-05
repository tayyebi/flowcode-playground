package engine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestEngine wires an Engine against real fcc/fcplay binaries.
//
// These tests exercise the actual toolchain rather than a stand-in, so they
// skip when it isn't built. Set FLOWCODE_FCC and FLOWCODE_RUNNER to run them
// locally; CI and the Docker image always have both.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()

	fcc := os.Getenv("FLOWCODE_FCC")
	runner := os.Getenv("FLOWCODE_RUNNER")
	if fcc == "" || runner == "" {
		t.Skip("set FLOWCODE_FCC and FLOWCODE_RUNNER to run the end-to-end tests")
	}
	for _, p := range []string{fcc, runner} {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			t.Skipf("%s is not an executable file", p)
		}
	}

	return New(fcc, runner, t.TempDir(), 5*time.Second, 5*time.Second, 64*1024, 2)
}

const helloSource = `workflow: HelloWorld

step greeting:
    emit
        value = "hello, world"
end

step saved:
    store set
        key = "greeting"
        value = greeting
end
`

func TestRunHelloWorld(t *testing.T) {
	e := newTestEngine(t)

	resp, err := e.Run(context.Background(), helloSource)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if resp.Compile.ExitCode != 0 {
		t.Fatalf("compile exit = %d, stderr: %s", resp.Compile.ExitCode, resp.Compile.Stderr)
	}
	if len(resp.Compile.Diagnostics) != 0 {
		t.Errorf("unexpected diagnostics: %+v", resp.Compile.Diagnostics)
	}

	if resp.Bytecode == nil {
		t.Fatal("no bytecode decoded")
	}
	if resp.Bytecode.InstructionCount != 2 {
		t.Errorf("InstructionCount = %d, want 2", resp.Bytecode.InstructionCount)
	}

	if resp.Run == nil {
		t.Fatal("run stage did not execute after a clean compile")
	}
	if resp.Run.ExitCode != 0 {
		t.Fatalf("run exit = %d, stderr: %s", resp.Run.ExitCode, resp.Run.Stderr)
	}

	// The whole reason fcplay exists: the stock CLI leaves the log level at
	// WARN and a successful run prints nothing at all. If this assertion ever
	// fails, the playground is showing users an empty pane.
	for _, want := range []string{"vm starting, 2 instructions", "vm completed successfully"} {
		if !strings.Contains(resp.Run.Stderr, want) {
			t.Errorf("run trace missing %q; got:\n%s", want, resp.Run.Stderr)
		}
	}
}

func TestCompileReportsWarningsWithLineNumbers(t *testing.T) {
	e := newTestEngine(t)

	// FlowCode has no comment syntax; this line compiles to a warning, not an
	// error, which is exactly why warnings have to reach the user.
	src := helloSource + "\n# this looks like a comment but is not\n"

	resp, err := e.Run(context.Background(), src)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if resp.Compile.ExitCode != 0 {
		t.Errorf("compile exit = %d, want 0 (warnings do not fail compilation)", resp.Compile.ExitCode)
	}
	if len(resp.Compile.Diagnostics) != 1 {
		t.Fatalf("got %d diagnostics, want 1: %+v", len(resp.Compile.Diagnostics), resp.Compile.Diagnostics)
	}
	d := resp.Compile.Diagnostics[0]
	if d.Level != "warning" {
		t.Errorf("Level = %q, want warning", d.Level)
	}
	if d.Line != 14 {
		t.Errorf("Line = %d, want 14", d.Line)
	}
	if resp.Run == nil {
		t.Error("a warning should not prevent the program from running")
	}
}

func TestCompileFailureSkipsRun(t *testing.T) {
	e := newTestEngine(t)

	// Two steps sharing a name is a hard error in the compiler's semantic pass.
	src := "workflow: Dup\n\nstep a:\n    emit\n        value = \"x\"\nend\n\nstep a:\n    emit\n        value = \"y\"\nend\n"

	resp, err := e.Run(context.Background(), src)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if resp.Compile.ExitCode == 0 {
		t.Fatalf("compile succeeded; expected a duplicate-step error. stderr: %s", resp.Compile.Stderr)
	}
	if resp.Run != nil {
		t.Error("run stage executed despite a failed compile")
	}

	var sawError bool
	for _, d := range resp.Compile.Diagnostics {
		if d.Level == "error" {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("no error diagnostic parsed from: %s", resp.Compile.Stderr)
	}
}

// TestWorkDirIsCleanedUp guards the tmpfs: a run that leaves its directory
// behind fills a 64MB mount over a day of traffic.
func TestWorkDirIsCleanedUp(t *testing.T) {
	e := newTestEngine(t)

	if _, err := e.Run(context.Background(), helloSource); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries, err := os.ReadDir(e.WorkDir)
	if err != nil {
		t.Fatalf("read work dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("work dir still holds %d entries, want none", len(entries))
	}
}

func TestRunBusyReturnsErrBusy(t *testing.T) {
	e := newTestEngine(t)
	e.QueueWait = 50 * time.Millisecond
	e.slots = make(chan struct{}, 1)
	e.slots <- struct{}{} // occupy the only slot

	_, err := e.Run(context.Background(), helloSource)
	if err != ErrBusy {
		t.Fatalf("Run error = %v, want ErrBusy", err)
	}
}
