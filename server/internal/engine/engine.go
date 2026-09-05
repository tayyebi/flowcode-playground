// Package engine compiles and runs FlowCode source through the sandboxed
// fcc/fcplay pipeline. It is the single execution path shared by the
// anonymous playground, saved projects, deployments, and triggers — nothing
// else in this codebase should shell out to fcc/fcplay directly.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const (
	sourceFile   = "main.fc"
	bytecodeFile = "main.fcb"
)

// ErrBusy is returned by Run when no execution slot freed up within
// QueueWait.
var ErrBusy = errors.New("engine: busy, all execution slots occupied")

// Engine owns the sandboxed compile+run pipeline and the bounded pool of
// concurrent executions shared by every caller: the anonymous playground,
// project runs, deployments, and triggers. Without this shared pool, a burst
// of requests across those paths could fork an unbounded number of
// compilers well before any individual rlimit was reached.
type Engine struct {
	CompilerPath string
	RunnerPath   string
	WorkDir      string
	Timeout      time.Duration
	QueueWait    time.Duration
	OutputLimit  int

	slots chan struct{}
}

// New builds an Engine. concurrency bounds how many compile+run pipelines can
// be in flight at once, across all callers combined.
func New(compilerPath, runnerPath, workDir string, timeout, queueWait time.Duration, outputLimit, concurrency int) *Engine {
	return &Engine{
		CompilerPath: compilerPath,
		RunnerPath:   runnerPath,
		WorkDir:      workDir,
		Timeout:      timeout,
		QueueWait:    queueWait,
		OutputLimit:  outputLimit,
		slots:        make(chan struct{}, concurrency),
	}
}

// CompileResult is StageResult plus what could be parsed out of it.
type CompileResult struct {
	StageResult
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Result is the outcome of compiling, and if the compile succeeded running,
// one FlowCode source. Field names/tags are load-bearing: they are the
// anonymous playground's public JSON contract and must not change shape.
type Result struct {
	Compile CompileResult `json:"compile"`
	// Bytecode is present whenever fcc produced a readable image — which
	// includes runs that only warned, since fcc still emits bytecode then.
	Bytecode      *Bytecode    `json:"bytecode,omitempty"`
	BytecodeError string       `json:"bytecodeError,omitempty"`
	Run           *StageResult `json:"run,omitempty"`
	Truncated     bool         `json:"truncated"`
}

// Run acquires a bounded execution slot and compiles+runs source.
//
// It returns ErrBusy if no slot freed up within QueueWait, and ctx.Err() if
// the caller's context was cancelled first — callers decide how to surface
// each (a 503 for an HTTP handler, a failed execution row for a trigger).
func (e *Engine) Run(ctx context.Context, source string) (*Result, error) {
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(e.QueueWait):
		return nil, ErrBusy
	}

	return e.compileAndRun(ctx, source)
}

func (e *Engine) compileAndRun(ctx context.Context, source string) (*Result, error) {
	dir, err := os.MkdirTemp(e.WorkDir, "run-")
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

	compile := runStage(ctx, e.Timeout, e.OutputLimit, dir, e.CompilerPath, sourceFile, bytecodeFile)
	res := &Result{
		Compile: CompileResult{
			StageResult: compile,
			Diagnostics: parseDiagnostics(compile.Stderr),
		},
	}

	// fcc writes bytecode best-effort even when it reports errors, so read it
	// regardless — a partial image is still worth showing in the bytecode pane.
	if data, err := os.ReadFile(filepath.Join(dir, bytecodeFile)); err == nil {
		if bc, derr := disassemble(data); derr == nil {
			res.Bytecode = bc
		} else {
			res.BytecodeError = derr.Error()
		}
	}

	// Only execute a clean compile. Running the partial output of a failed one
	// would report VM errors that are really just compiler errors in disguise.
	if compile.ExitCode == 0 && !compile.TimedOut {
		run := runStage(ctx, e.Timeout, e.OutputLimit, dir, e.RunnerPath, bytecodeFile)
		res.Run = &run
	}

	res.Truncated = res.Compile.Truncated || (res.Run != nil && res.Run.Truncated)
	return res, nil
}
