package engine

import (
	"regexp"
	"strconv"
	"strings"
)

// Diagnostic is one compiler message, positioned so the editor can mark it.
type Diagnostic struct {
	Level   string `json:"level"` // "error" or "warning"
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// fcc writes diagnostics to stderr as `<level>: line <n>: <message>`
// (src/compiler.c, the FC_COMPILER_MAIN block).
var diagnosticRe = regexp.MustCompile(`^(error|warning): line (\d+): (.*)$`)

// parseDiagnostics pulls structured diagnostics out of fcc's stderr.
//
// Lines that don't match are left alone rather than guessed at: fcc also emits
// unpositioned summaries ("compilation completed with 2 diagnostic(s)") and
// I/O errors, and the raw stderr is returned to the client alongside this, so
// nothing is lost by skipping them here.
//
// Note that a warning is not a non-event in this language. FlowCode has no
// comment syntax, and any line the parser doesn't recognise — a typo, a stray
// note — becomes `warning: unrecognized line` while compilation still exits 0.
// Surfacing warnings as prominently as errors is what stops a silently ignored
// line from looking like a working program.
func parseDiagnostics(stderr string) []Diagnostic {
	diags := []Diagnostic{}
	for _, line := range strings.Split(stderr, "\n") {
		m := diagnosticRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		diags = append(diags, Diagnostic{
			Level:   m[1],
			Line:    n,
			Message: m[3],
		})
	}
	return diags
}
