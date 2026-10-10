// Summary flattens a Result into the store's execution-record shape, so the
// store layer never has to import the engine.
package engine

import (
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

func (r *Result) Summary() *store.RunSummary {
	if r == nil {
		return nil
	}
	s := &store.RunSummary{
		CompileExitCode:   r.Compile.ExitCode,
		CompileStderr:     r.Compile.Stderr,
		CompileTimedOut:   r.Compile.TimedOut,
		CompileDurationMs: r.Compile.DurationMs,
		Truncated:         r.Truncated,
	}
	if r.Run != nil {
		s.HasRun = true
		s.RunExitCode = r.Run.ExitCode
		s.RunStderr = r.Run.Stderr
		s.RunTimedOut = r.Run.TimedOut
		s.RunDurationMs = r.Run.DurationMs
	}
	return s
}
