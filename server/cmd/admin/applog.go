package main

// The app log viewer: tails the app's own log file (data/logs/<app>.log) — the
// operational record the playground appends at every app-call dispatch. The
// admin area never reads Loki: Loki is the Logger app's external backend (as
// the mail relay is the Mail app's) and holds user log entries only, and the
// platform's own logs live on its own stdout, not in any admin page.

import (
	"strings"

	"github.com/tayyebi/flowcode-playground/server/internal/applogs"
)

// tailAppLog returns the newest entries from the app's own log file, newest
// first, optionally narrowed to a user (substring, case-insensitive — the
// emails are label-safe enough for a substring match).
func tailAppLog(app, userFilter string) []applogs.Entry {
	entries := applogs.Tail(applogs.Dir(), app, 100)
	if userFilter == "" {
		return entries
	}
	needle := strings.ToLower(strings.TrimSpace(userFilter))
	out := []applogs.Entry{}
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.User), needle) {
			out = append(out, e)
		}
	}
	return out
}
