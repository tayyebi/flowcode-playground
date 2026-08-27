package main

import (
	"reflect"
	"testing"
)

func TestParseDiagnostics(t *testing.T) {
	// Verbatim fcc stderr for a source with two unrecognised lines. FlowCode
	// has no comment syntax, so both `#` and `//` land here as warnings while
	// compilation still succeeds — the case that makes warnings worth surfacing.
	stderr := "warning: line 2: unrecognized line: # a hash comment\n" +
		"warning: line 3: unrecognized line: // a slash comment\n" +
		"compilation completed with 2 diagnostic(s)\n"

	got := parseDiagnostics(stderr)
	want := []Diagnostic{
		{Level: "warning", Line: 2, Message: "unrecognized line: # a hash comment"},
		{Level: "warning", Line: 3, Message: "unrecognized line: // a slash comment"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseDiagnostics = %#v, want %#v", got, want)
	}
}

func TestParseDiagnosticsErrors(t *testing.T) {
	got := parseDiagnostics("error: line 12: duplicate step name 'greeting'\n")
	if len(got) != 1 {
		t.Fatalf("got %d diagnostics, want 1", len(got))
	}
	if got[0].Level != "error" || got[0].Line != 12 {
		t.Errorf("got %+v, want an error on line 12", got[0])
	}
}

func TestParseDiagnosticsIgnoresUnpositioned(t *testing.T) {
	// These reach the client through the raw stderr field instead; inventing a
	// line number for them would put a bogus marker in the editor.
	stderr := "error: cannot open missing.fc\ncompilation completed with errors\n"
	if got := parseDiagnostics(stderr); len(got) != 0 {
		t.Errorf("got %#v, want none", got)
	}
}

func TestParseDiagnosticsEmpty(t *testing.T) {
	// Must be an empty slice, not nil: it is serialised straight to JSON and
	// the frontend expects [] rather than null.
	got := parseDiagnostics("")
	if got == nil {
		t.Fatal("got nil, want an empty slice")
	}
	if len(got) != 0 {
		t.Errorf("got %d diagnostics, want 0", len(got))
	}
}

func TestParseDiagnosticsHandlesCRLF(t *testing.T) {
	got := parseDiagnostics("warning: line 4: unrecognized line: x\r\n")
	if len(got) != 1 {
		t.Fatalf("got %d diagnostics, want 1", len(got))
	}
	if got[0].Message != "unrecognized line: x" {
		t.Errorf("Message = %q, carriage return not trimmed", got[0].Message)
	}
}
