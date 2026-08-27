package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestServer wires a Server against real fcc/fcplay binaries.
//
// These tests exercise the actual toolchain rather than a stand-in, so they
// skip when it isn't built. Set FLOWCODE_FCC and FLOWCODE_RUNNER to run them
// locally; CI and the Docker image always have both.
func newTestServer(t *testing.T) *Server {
	t.Helper()

	fcc := os.Getenv("FLOWCODE_FCC")
	runner := os.Getenv("FLOWCODE_RUNNER")
	if fcc == "" || runner == "" {
		t.Skip("set FLOWCODE_FCC and FLOWCODE_RUNNER to run the end-to-end tests")
	}
	for _, p := range []string{fcc, runner} {
		if err := checkExecutable(p); err != nil {
			t.Skipf("%v", err)
		}
	}

	return &Server{
		CompilerPath: fcc,
		RunnerPath:   runner,
		WorkDir:      t.TempDir(),
		Timeout:      5 * time.Second,
		QueueWait:    5 * time.Second,
		slots:        make(chan struct{}, 2),
		limiter:      newRateLimiter(10000, 10000, time.Minute),
	}
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

func TestCompileAndRunHelloWorld(t *testing.T) {
	s := newTestServer(t)

	resp, err := s.compileAndRun(context.Background(), helloSource)
	if err != nil {
		t.Fatalf("compileAndRun: %v", err)
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
	s := newTestServer(t)

	// FlowCode has no comment syntax; this line compiles to a warning, not an
	// error, which is exactly why warnings have to reach the user.
	src := helloSource + "\n# this looks like a comment but is not\n"

	resp, err := s.compileAndRun(context.Background(), src)
	if err != nil {
		t.Fatalf("compileAndRun: %v", err)
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
	s := newTestServer(t)

	// Two steps sharing a name is a hard error in the compiler's semantic pass.
	src := "workflow: Dup\n\nstep a:\n    emit\n        value = \"x\"\nend\n\nstep a:\n    emit\n        value = \"y\"\nend\n"

	resp, err := s.compileAndRun(context.Background(), src)
	if err != nil {
		t.Fatalf("compileAndRun: %v", err)
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
	s := newTestServer(t)

	if _, err := s.compileAndRun(context.Background(), helloSource); err != nil {
		t.Fatalf("compileAndRun: %v", err)
	}

	entries, err := os.ReadDir(s.WorkDir)
	if err != nil {
		t.Fatalf("read work dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("work dir still holds %d entries, want none", len(entries))
	}
}

func TestHandleRunRejectsOversizedSource(t *testing.T) {
	s := newTestServer(t)

	body, _ := json.Marshal(runRequest{Source: strings.Repeat("x", maxSourceBytes+1)})
	req := httptest.NewRequest(http.MethodPost, "/api/run", bytes.NewReader(body))
	req.RemoteAddr = "192.0.2.1:1234"
	rec := httptest.NewRecorder()

	s.handleRun(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestHandleRunRejectsEmptyAndMalformed(t *testing.T) {
	s := newTestServer(t)

	for _, tt := range []struct {
		name string
		body string
	}{
		{"empty source", `{"source":""}`},
		{"not json", `not json at all`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/run", strings.NewReader(tt.body))
			req.RemoteAddr = "192.0.2.1:1234"
			rec := httptest.NewRecorder()

			s.handleRun(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// TestHandleRunReportsFailuresAs200 pins the API contract: a program that does
// not compile is a normal playground outcome and its details belong in the
// body, not in an HTTP error the frontend would have to special-case.
func TestHandleRunReportsFailuresAs200(t *testing.T) {
	s := newTestServer(t)

	body, _ := json.Marshal(runRequest{Source: "this is not a workflow at all\n"})
	req := httptest.NewRequest(http.MethodPost, "/api/run", bytes.NewReader(body))
	req.RemoteAddr = "192.0.2.1:1234"
	rec := httptest.NewRecorder()

	s.handleRun(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp runResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Compile.Diagnostics) == 0 {
		t.Error("no diagnostics returned for unrecognised source")
	}
}

// TestAllSamplesCompileAndRun mirrors upstream's tests/test_samples.sh: every
// program the picker offers must work, or the playground ships a broken menu.
func TestAllSamplesCompileAndRun(t *testing.T) {
	s := newTestServer(t)

	dir := os.Getenv("FLOWCODE_SAMPLES_DIR")
	if dir == "" {
		t.Skip("set FLOWCODE_SAMPLES_DIR to check the bundled samples")
	}
	samples, err := loadSamples(dir)
	if err != nil {
		t.Fatalf("loadSamples: %v", err)
	}

	for _, sample := range samples {
		t.Run(sample.ID, func(t *testing.T) {
			resp, err := s.compileAndRun(context.Background(), sample.Source)
			if err != nil {
				t.Fatalf("compileAndRun: %v", err)
			}
			if resp.Compile.ExitCode != 0 {
				t.Fatalf("compile failed: %s", resp.Compile.Stderr)
			}
			// Upstream's own harness treats an unrecognised line as a failure;
			// a sample that warns means our snapshot has drifted from theirs.
			for _, d := range resp.Compile.Diagnostics {
				t.Errorf("unexpected %s on line %d: %s", d.Level, d.Line, d.Message)
			}
			if resp.Run == nil || resp.Run.ExitCode != 0 {
				t.Fatalf("run failed: %+v", resp.Run)
			}
			if !strings.Contains(resp.Run.Stderr, "vm completed successfully") {
				t.Errorf("run did not complete; trace:\n%s", resp.Run.Stderr)
			}
		})
	}
}
