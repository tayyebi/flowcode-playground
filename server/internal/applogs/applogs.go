// Package applogs gives each app its own operational log file. Every bridge
// call the playground dispatches — success or failure, including disabled /
// unconfigured / quota-rejected calls — appends one JSON line to
// <dir>/<app>.log (default data/logs/<app>.log), and the administration
// area's Logs pages tail those
// files.
//
// The files hold the APP's operational record only, never user payloads of
// the Logger app: user log lines live in Loki (the Logger app's external
// backend, as the mail relay is the Mail app's), exactly as mail bodies live
// in the mail system. Platform services themselves log to their own
// stdout/stderr and never appear here or in Loki.
//
// Both binaries run in /src with the repo bind-mounted from the host, so the
// default directory is shared between the writer (playground) and the reader
// (admin) with no compose plumbing.
package applogs

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry is one operational log line (JSONL in <app>.log).
type Entry struct {
	Time       string `json:"time"` // RFC3339Nano UTC
	Name       string `json:"name"` // e.g. "MailApp.sendEmail", "Logger.log"
	User       string `json:"user,omitempty"`
	ProjectID  int64  `json:"project,omitempty"`
	ExecID     int64  `json:"exec,omitempty"`
	Status     string `json:"status"` // "sent to …" / "error: …"
	DurationMs int64  `json:"durationMs,omitempty"`
	Request    string `json:"request,omitempty"`  // capped summary; never set for the Logger app
	Response   string `json:"response,omitempty"` // capped summary; never set for the Logger app
}

// Dir is where the app log files live: APP_LOG_DIR, default "data/logs"
// (relative to the binary's working directory, i.e. the bind-mounted repo
// root — the git-ignored data/ directory docker mounts from the host).
func Dir() string {
	if d := os.Getenv("APP_LOG_DIR"); d != "" {
		return d
	}
	return "data/logs"
}

// trimAt / keepLines: when a file reaches trimAt lines it is rewritten down
// to the newest keepLines — a bounded, restart-stable operational record in
// the same spirit as the throw-away mocks' capped JSON stores.
const (
	trimAt    = 5000
	keepLines = 2500
)

// Logger appends entries for one app family to its log file. Only the
// playground writes; the admin binary reads via Tail. Safe for concurrent
// use; every failure is best-effort (logged to stderr, never propagated into
// the app call it is recording).
type Logger struct {
	mu    sync.Mutex
	path  string
	lines int // lines currently in the file, counted at startup
}

// New returns the Logger for one app ("mail", "http", "logger").
func New(app string) *Logger {
	path := filepath.Join(Dir(), app+".log")
	return &Logger{path: path, lines: countLines(path)}
}

// Append writes one entry, timestamping it when the caller did not.
func (l *Logger) Append(e Entry) {
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		log.Printf("applogs %s: marshal: %v", l.path, err)
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := writeFileLine(l.path, raw); err != nil {
		log.Printf("applogs %s: %v", l.path, err)
		return
	}
	l.lines++
	if l.lines > trimAt {
		l.trim()
	}
}

// trim rewrites the file down to the newest keepLines lines. Called with the
// mutex held; the tmp-file rename keeps a concurrent Tail on the old inode
// consistent until its next open.
func (l *Logger) trim() {
	lines := readLines(l.path)
	if len(lines) > keepLines {
		lines = lines[len(lines)-keepLines:]
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		log.Printf("applogs %s: trim write: %v", l.path, err)
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		log.Printf("applogs %s: trim rename: %v", l.path, err)
		return
	}
	l.lines = len(lines)
}

// Tail returns up to n entries from dir/<app>.log, newest first. A missing
// file is an empty log, not an error; unparsable lines (a torn trailing line
// from a concurrent append) are skipped.
func Tail(dir, app string, n int) []Entry {
	raw, err := os.ReadFile(filepath.Join(dir, app+".log"))
	if err != nil {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	out := []Entry{}
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

/* ------------------------------------------------------------------ */
/* File helpers                                                        */
/* ------------------------------------------------------------------ */

func writeFileLine(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

func readLines(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	raw = []byte(strings.TrimRight(string(raw), "\n"))
	if len(raw) == 0 {
		return nil
	}
	return strings.Split(string(raw), "\n")
}

func countLines(path string) int {
	return len(readLines(path))
}
