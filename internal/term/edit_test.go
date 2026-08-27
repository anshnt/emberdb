package term

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLineEditing(t *testing.T) {
	var l line
	for _, r := range "hello world" {
		l.insert(r)
	}
	if l.String() != "hello world" {
		t.Fatalf("line = %q", l.String())
	}
	l.backspace()
	if l.String() != "hello worl" {
		t.Fatalf("after backspace: %q", l.String())
	}
	l.deleteWord()
	if l.String() != "hello " {
		t.Fatalf("after deleting a word: %q", l.String())
	}
	l.deleteWord()
	if l.String() != "" {
		t.Fatalf("after deleting the last word: %q", l.String())
	}

	l.set("abc")
	l.cursor = 1
	l.insert('X')
	if l.String() != "aXbc" || l.cursor != 2 {
		t.Fatalf("insert in the middle gave %q with the cursor at %d", l.String(), l.cursor)
	}
	l.deleteForward()
	if l.String() != "aXc" {
		t.Fatalf("delete forward gave %q", l.String())
	}
	l.cursor = 0
	l.backspace()
	if l.String() != "aXc" {
		t.Fatalf("backspace at the start of the line changed it to %q", l.String())
	}
	l.deleteForward()
	if l.String() != "Xc" {
		t.Fatalf("delete forward at the start gave %q", l.String())
	}
	l.cursor = len(l.runes)
	l.deleteForward()
	if l.String() != "Xc" {
		t.Fatalf("delete forward at the end changed the line to %q", l.String())
	}
}

func TestLineEditingIsRuneAware(t *testing.T) {
	var l line
	for _, r := range "日本語" {
		l.insert(r)
	}
	if l.cursor != 3 {
		t.Fatalf("cursor is at %d after three multi-byte characters, want 3", l.cursor)
	}
	l.backspace()
	if l.String() != "日本" {
		t.Fatalf("backspace over a multi-byte character gave %q", l.String())
	}
}

func TestHistoryRecall(t *testing.T) {
	e := &Editor{}
	e.AddHistory("first")
	e.AddHistory("second")
	e.AddHistory("second") // an immediate repeat is not recorded twice
	e.AddHistory("   ")    // nor is a blank line
	if len(e.History()) != 2 {
		t.Fatalf("history = %q", e.History())
	}

	var l line
	l.set("half typed")
	e.browsing = len(e.history)
	e.recall(&l, -1)
	if l.String() != "second" {
		t.Fatalf("one step back gave %q", l.String())
	}
	e.recall(&l, -1)
	if l.String() != "first" {
		t.Fatalf("two steps back gave %q", l.String())
	}
	e.recall(&l, -1)
	if l.String() != "first" {
		t.Fatalf("stepping past the oldest entry gave %q", l.String())
	}
	e.recall(&l, +1)
	e.recall(&l, +1)
	if l.String() != "half typed" {
		t.Fatalf("stepping back to the present gave %q, want the stashed line", l.String())
	}
	e.recall(&l, +1)
	if l.String() != "half typed" {
		t.Fatalf("stepping past the present gave %q", l.String())
	}
}

func TestHistoryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	e := &Editor{}
	if err := e.LoadHistory(path); err != nil {
		t.Fatalf("loading a history file that does not exist: %v", err)
	}
	for i := 0; i < 10; i++ {
		e.AddHistory(strings.Repeat("x", i+1))
	}
	if err := e.SaveHistory(path, 4); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}
	reloaded := &Editor{}
	if err := reloaded.LoadHistory(path); err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if got := reloaded.History(); len(got) != 4 || got[0] != strings.Repeat("x", 7) {
		t.Fatalf("reloaded history = %q, want the last four entries", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("history file mode is %v, want 0600: it can hold anything the user typed", perm)
	}
}

func TestEditorFallsBackToPlainLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("one\ntwo\nno trailing newline"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	var out strings.Builder
	e := NewEditor(f, &out)
	if e.Interactive() {
		t.Fatal("a file should not be treated as a terminal")
	}
	for _, want := range []string{"one", "two", "no trailing newline"} {
		got, err := e.ReadLine("prompt> ")
		if err != nil {
			t.Fatalf("ReadLine: %v", err)
		}
		if got != want {
			t.Fatalf("ReadLine = %q, want %q", got, want)
		}
	}
	if _, err := e.ReadLine(""); err == nil {
		t.Fatal("reading past the end of the file should report an error")
	}
	// The editor prints whatever prompt it is handed, in either mode; it is
	// the caller that decides not to prompt when the input is a pipe.
	if got := out.String(); got != strings.Repeat("prompt> ", 3) {
		t.Errorf("prompts written = %q", got)
	}
}
