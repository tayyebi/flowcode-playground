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
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/apps"
)

const (
	sourceFile   = "main.fc"
	bytecodeFile = "main.fcb"
)

// ErrBusy is returned by Run when no execution slot freed up within
// QueueWait.
var ErrBusy = errors.New("engine: busy, all execution slots occupied")

// Engine owns the sandboxed compile+run pipeline and the bounded pool of
// concurrent executions shared by every caller: the project runs,
// deployments, and triggers. Without this shared pool, a burst of requests
// across those paths could fork an unbounded number of compilers well before
// any individual rlimit was reached.
//
// Apps, when set, enables the unix-socket bridge: the runner receives the
// socket path as a second argument and forwards every MailApp/UrlFetchApp/
// Logger.log call back here for execution.
type Engine struct {
	CompilerPath string
	RunnerPath   string
	WorkDir      string
	Timeout      time.Duration
	QueueWait    time.Duration
	OutputLimit  int

	Apps *apps.Service

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
// public JSON contract and must not change shape.
type Result struct {
	Compile CompileResult `json:"compile"`
	// Bytecode is present whenever fcc produced a readable image — which
	// includes runs that only warned, since fcc still emits bytecode then.
	Bytecode      *Bytecode    `json:"bytecode,omitempty"`
	BytecodeError string       `json:"bytecodeError,omitempty"`
	Run           *StageResult `json:"run,omitempty"`
	// AppCalls is every bridge invocation this run made, in order — the
	// GAS-style execution transcript for the four apps.
	AppCalls  []*AppCall `json:"appCalls,omitempty"`
	Truncated bool       `json:"truncated"`
}

// AppCall is one bridge invocation, for display in the run result and (for
// mail/http) persistence into the app_calls table.
type AppCall struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Request    string `json:"request,omitempty"`
	Response   string `json:"response,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

// Run acquires a bounded execution slot and compiles+runs source with no
// attributing actor (bridge disabled unless Engine.Apps is set).
//
// It returns ErrBusy if no slot freed up within QueueWait, and ctx.Err() if
// the caller's context was cancelled first — callers decide how to surface
// each (a 503 for an HTTP handler, a failed execution row for a trigger).
func (e *Engine) Run(ctx context.Context, source string) (*Result, error) {
	return e.RunWithActor(ctx, source, apps.Actor{})
}

// RunWithActor is Run with the user a run is executed as: quotas, call
// records, and Loki labels attribute to the actor. A zero actor with Apps
// set still bridges (attributed to uid 0).
func (e *Engine) RunWithActor(ctx context.Context, source string, actor apps.Actor) (*Result, error) {
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(e.QueueWait):
		return nil, ErrBusy
	}

	return e.compileAndRun(ctx, source, actor)
}

func (e *Engine) compileAndRun(ctx context.Context, source string, actor apps.Actor) (*Result, error) {
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
		runArgs := []string{bytecodeFile}
		if e.Apps != nil {
			// Bridge mode: a unix socket in the run directory, served for
			// exactly as long as the runner lives. The runner forwards its
			// app calls here; the core itself never listens to anything.
			ln, lerr := net.Listen("unix", bridgeSocketPath(dir))
			if lerr == nil {
				defer ln.Close()
				runArgs = append(runArgs, bridgeSocketPath(dir))
				go e.serveBridge(ctx, ln, actor, res)
			} else {
				log.Printf("bridge disabled for this run: %v", lerr)
			}
		}
		run := runStage(ctx, e.Timeout, e.OutputLimit, dir, e.RunnerPath, runArgs...)
		res.Run = &run
	}

	res.Truncated = res.Compile.Truncated || (res.Run != nil && res.Run.Truncated)
	return res, nil
}
