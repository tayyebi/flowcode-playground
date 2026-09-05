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

	"github.com/tayyebi/flowcode-playground/server/internal/engine"
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
		engine:  engine.New(fcc, runner, t.TempDir(), 5*time.Second, 5*time.Second, maxOutputBytes, 2),
		limiter: newRateLimiter(10000, 10000, time.Minute),
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
	var resp engine.Result
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
			resp, err := s.engine.Run(context.Background(), sample.Source)
			if err != nil {
				t.Fatalf("Run: %v", err)
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
