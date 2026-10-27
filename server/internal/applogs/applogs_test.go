package applogs

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestLogger(t *testing.T) *Logger {
	t.Helper()
	return &Logger{path: filepath.Join(t.TempDir(), "mail.log")}
}

func TestAppendThenTailNewestFirst(t *testing.T) {
	l := newTestLogger(t)
	l.Append(Entry{Name: "MailApp.sendEmail", User: "a@example.com", Status: "sent to b@example.com"})
	l.Append(Entry{Name: "MailApp.read", User: "a@example.com", Status: "3 messages from INBOX"})

	got := Tail(filepath.Dir(l.path), "mail", 100)
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if got[0].Name != "MailApp.read" || got[1].Name != "MailApp.sendEmail" {
		t.Fatalf("want newest first, got %s then %s", got[1].Name, got[0].Name)
	}
	if got[0].Time == "" {
		t.Fatal("Append must timestamp entries")
	}
}

func TestTailMissingFileIsEmpty(t *testing.T) {
	if got := Tail(t.TempDir(), "http", 100); got != nil {
		t.Fatalf("missing file must tail empty, got %v", got)
	}
}

func TestTailTornTrailingLine(t *testing.T) {
	l := newTestLogger(t)
	l.Append(Entry{Name: "UrlFetchApp.fetch", Status: "200 OK"})

	// A concurrent append can leave a torn last line behind.
	raw, _ := os.ReadFile(l.path)
	os.WriteFile(l.path, append(raw, []byte(`{"name":"UrlFetchApp.fe`)...), 0o644)

	got := Tail(filepath.Dir(l.path), "mail", 100)
	if len(got) != 1 || got[0].Name != "UrlFetchApp.fetch" {
		t.Fatalf("torn trailing line must be skipped, got %v", got)
	}
}

func TestTrimCapsAtKeepLines(t *testing.T) {
	l := &Logger{path: filepath.Join(t.TempDir(), "mail.log")}
	for i := 0; i < trimAt; i++ {
		if err := writeFileLine(l.path, []byte(`{"name":"old","status":"x"}`)); err != nil {
			t.Fatal(err)
		}
		l.lines++
	}
	l.Append(Entry{Name: "newest", Status: "x"}) // crosses trimAt → trim

	if l.lines != keepLines {
		t.Fatalf("after crossing trimAt the file must hold keepLines lines, got %d", l.lines)
	}
	if got := len(readLines(l.path)); got != keepLines {
		t.Fatalf("file must be rewritten to keepLines lines, got %d", got)
	}
	entries := Tail(filepath.Dir(l.path), "mail", 3)
	if len(entries) != 3 || entries[0].Name != "newest" {
		t.Fatalf("trim must keep the newest entries, got newest=%q", entries[0].Name)
	}
}
