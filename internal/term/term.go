// Package term provides the terminal handling the ember command needs: raw
// mode, and a line editor with history.
//
// It deliberately depends on nothing outside the standard library. emberdb's
// core module has no third-party dependencies, and the CLI ships inside it, so
// the few hundred bytes of ioctl plumbing here are the price of keeping that
// true.
package term

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
)

// ErrInterrupted reports that the user pressed Ctrl-C. The caller should
// abandon the current line and prompt again.
var ErrInterrupted = errors.New("interrupted")

// Editor reads lines from a terminal with history and in-line editing.
//
// On a terminal it takes over the input in raw mode and interprets keys
// itself. When the input is a pipe or a file, it falls back to reading whole
// lines, so the same code path serves both a REPL and a script.
type Editor struct {
	in       *os.File
	out      io.Writer
	fallback *bufio.Reader
	raw      bool

	history []string
	// browsing is the index the up and down keys are currently at, equal to
	// len(history) when the user is on the line they are typing.
	browsing int
	// stash keeps the partially typed line while the user browses history.
	stash string
}

// NewEditor returns an editor reading from in and echoing to out. When in is
// not a terminal, the editor reads plain lines instead.
func NewEditor(in *os.File, out io.Writer) *Editor {
	e := &Editor{in: in, out: out, raw: IsTerminal(in)}
	if !e.raw {
		e.fallback = bufio.NewReader(in)
	}
	return e
}

// Interactive reports whether the editor is driving a real terminal.
func (e *Editor) Interactive() bool { return e.raw }

// History returns the lines the editor has recorded, oldest first.
func (e *Editor) History() []string { return e.history }

// AddHistory records a line, ignoring blanks and immediate repeats.
func (e *Editor) AddHistory(line string) {
	line = strings.TrimRight(line, "\n")
	if strings.TrimSpace(line) == "" {
		return
	}
	if n := len(e.history); n > 0 && e.history[n-1] == line {
		e.browsing = n
		return
	}
	e.history = append(e.history, line)
	e.browsing = len(e.history)
}

// LoadHistory reads a history file, ignoring one that does not exist.
func (e *Editor) LoadHistory(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		e.AddHistory(line)
	}
	return nil
}

// SaveHistory writes the most recent entries to a file, capped at limit lines
// so it cannot grow without bound.
func (e *Editor) SaveHistory(path string, limit int) error {
	lines := e.history
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	if len(lines) == 0 {
		return nil
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// ReadLine prompts and returns one line without its terminator. It returns
// io.EOF at the end of the input and ErrInterrupted on Ctrl-C.
//
// The prompt is written in either mode. A caller reading from a pipe usually
// passes an empty one.
func (e *Editor) ReadLine(prompt string) (string, error) {
	if !e.raw {
		return e.readPlain(prompt)
	}
	restore, err := MakeRaw(e.in)
	if err != nil {
		// The terminal refused raw mode, so fall back rather than fail.
		e.raw = false
		e.fallback = bufio.NewReader(e.in)
		return e.readPlain(prompt)
	}
	defer restore()
	return e.readRaw(prompt)
}

// readPlain reads a line without any editing, for pipes and files.
func (e *Editor) readPlain(prompt string) (string, error) {
	if prompt != "" {
		io.WriteString(e.out, prompt)
	}
	line, err := e.fallback.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil {
		if errors.Is(err, io.EOF) && line != "" {
			return line, nil
		}
		return line, err
	}
	return line, nil
}
