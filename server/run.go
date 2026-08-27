package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	maxSourceBytes = 64 * 1024
	maxOutputBytes = 64 * 1024
	sourceFile     = "main.fc"
	bytecodeFile   = "main.fcb"
)

type runRequest struct {
	Source string `json:"source"`
}

// CompileResult is StageResult plus what we could parse out of it.
type CompileResult struct {
	StageResult
	Diagnostics []Diagnostic `json:"diagnostics"`
}

type runResponse struct {
	Compile CompileResult `json:"compile"`
	// Bytecode is present whenever fcc produced a readable image — which
	// includes runs that only warned, since fcc still emits bytecode then.
	Bytecode      *Bytecode    `json:"bytecode,omitempty"`
	BytecodeError string       `json:"bytecodeError,omitempty"`
	Run           *StageResult `json:"run,omitempty"`
	Truncated     bool         `json:"truncated"`
}

// handleRun compiles and runs one submission.
//
// Failures at every stage are reported as a 200 with the details in the body:
// a program that doesn't compile is a normal, expected outcome of using a
// playground, not an HTTP error. Non-2xx is reserved for the request itself
// being unacceptable — too large, malformed, rate limited, or the server being
// unable to do the work at all.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Cap the body before decoding so an oversized upload is refused as it
	// arrives rather than after it has been buffered in full.
	r.Body = http.MaxBytesReader(w, r.Body, maxSourceBytes+1024)

	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("source must be at most %d bytes", maxSourceBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "request body must be JSON: {\"source\": \"...\"}")
		return
	}
	if len(req.Source) > maxSourceBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("source must be at most %d bytes", maxSourceBytes))
		return
	}
	if req.Source == "" {
		writeError(w, http.StatusBadRequest, "source is empty")
		return
	}

	// Bound how many compilations run at once. Without this a burst of requests
	// would fork an unbounded number of compilers and the box would fall over
	// well before any individual rlimit was reached.
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-r.Context().Done():
		return
	case <-time.After(s.QueueWait):
		writeError(w, http.StatusServiceUnavailable, "playground is busy, try again shortly")
		return
	}

	resp, err := s.compileAndRun(r.Context(), req.Source)
	if err != nil {
		log.Printf("run failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not execute the program")
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) compileAndRun(ctx context.Context, source string) (*runResponse, error) {
	dir, err := os.MkdirTemp(s.WorkDir, "run-")
	if err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	// Every run gets a fresh directory and takes it with it when it goes,
	// including on timeout — otherwise a tmpfs fills up over a day of traffic.
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("could not clean up %s: %v", dir, err)
		}
	}()

	srcPath := filepath.Join(dir, sourceFile)
	if err := os.WriteFile(srcPath, []byte(source), 0o600); err != nil {
		return nil, fmt.Errorf("write source: %w", err)
	}

	compile := runStage(ctx, s.Timeout, maxOutputBytes, dir, s.CompilerPath, sourceFile, bytecodeFile)
	resp := &runResponse{
		Compile: CompileResult{
			StageResult: compile,
			Diagnostics: parseDiagnostics(compile.Stderr),
		},
	}

	// fcc writes bytecode best-effort even when it reports errors, so read it
	// regardless — a partial image is still worth showing in the bytecode pane.
	if data, err := os.ReadFile(filepath.Join(dir, bytecodeFile)); err == nil {
		if bc, derr := disassemble(data); derr == nil {
			resp.Bytecode = bc
		} else {
			resp.BytecodeError = derr.Error()
		}
	}

	// Only execute a clean compile. Running the partial output of a failed one
	// would report VM errors that are really just compiler errors in disguise.
	if compile.ExitCode == 0 && !compile.TimedOut {
		run := runStage(ctx, s.Timeout, maxOutputBytes, dir, s.RunnerPath, bytecodeFile)
		resp.Run = &run
	}

	resp.Truncated = resp.Compile.Truncated || (resp.Run != nil && resp.Run.Truncated)
	return resp, nil
}
