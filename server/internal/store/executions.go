package store

// Execution is one recorded run of a project file, from any of the three
// sources that can trigger one: a manual project-run, a deployment request,
// or a scheduled trigger. ActorID is the user the run was attributed to.
// Rows live in ws_executions.
type Execution struct {
	ID              int64  `json:"id"`
	ProjectID       int64  `json:"projectId"`
	FileID          *int64 `json:"fileId,omitempty"`
	FileName        string `json:"fileName"`
	VersionID       *int64 `json:"versionId,omitempty"`
	Source          string `json:"source"` // 'project-run' | 'deployment' | 'trigger'
	DeploymentID    *int64 `json:"deploymentId,omitempty"`
	TriggerID       *int64 `json:"triggerId,omitempty"`
	ActorID         *int64 `json:"actorId,omitempty"`
	StartedAt       string `json:"startedAt"`
	FinishedAt      string `json:"finishedAt,omitempty"`
	DurationMs      *int64 `json:"durationMs,omitempty"`
	CompileExitCode *int   `json:"compileExitCode,omitempty"`
	CompileStderr   string `json:"compileStderr,omitempty"`
	RunExitCode     *int   `json:"runExitCode,omitempty"`
	RunStderr       string `json:"runStderr,omitempty"`
	TimedOut        bool   `json:"timedOut"`
	Truncated       bool   `json:"truncated"`
	Error           string `json:"error,omitempty"`
}

// RunSummary is the flattened outcome of one engine run — the parts the
// executions table records. The engine maps its own Result into this so the
// store layer does not depend on it.
type RunSummary struct {
	CompileExitCode   int
	CompileStderr     string
	CompileTimedOut   bool
	CompileDurationMs int64
	HasRun            bool
	RunExitCode       int
	RunStderr         string
	RunTimedOut       bool
	RunDurationMs     int64
	Truncated         bool
}

// NewExecution is what a caller records after a run returns (or fails
// outright, in which case Summary is nil and Err carries the reason).
type NewExecution struct {
	ProjectID    int64
	FileID       *int64
	FileName     string
	VersionID    *int64
	Source       string
	DeploymentID *int64
	TriggerID    *int64
	StartedAt    string
	FinishedAt   string
	Summary      *RunSummary
	Err          error
}
