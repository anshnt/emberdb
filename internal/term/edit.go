package term

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Control characters the editor acts on.
const (
	keyCtrlA     = 1
	keyCtrlB     = 2
	keyCtrlC     = 3
	keyCtrlD     = 4
	keyCtrlE     = 5
	keyCtrlF     = 6
	keyBackspace = 8
	keyTab       = 9
	keyEnter     = 13
	keyCtrlK     = 11
	keyCtrlL     = 12
	keyCtrlN     = 14
	keyCtrlP     = 16
	keyCtrlU     = 21
	keyCtrlW     = 23
	keyEscape    = 27
	keyDelete    = 127
)

// line is the text being edited and where the cursor sits in it, both in
// runes so that multi-byte characters move the cursor by one.
type line struct {
	runes  []rune
	cursor int
}

// String returns the text being edited.
func (l *line) String() string { return string(l.runes) }

func (l *line) insert(r rune) {
	l.runes = append(l.runes, 0)
	copy(l.runes[l.cursor+1:], l.runes[l.cursor:])
	l.runes[l.cursor] = r
	l.cursor++
}

func (l *line) backspace() {
	if l.cursor == 0 {
		return
	}
	l.runes = append(l.runes[:l.cursor-1], l.runes[l.cursor:]...)
	l.cursor--
}

func (l *line) deleteForward() {
	if l.cursor >= len(l.runes) {
		return
	}
	l.runes = append(l.runes[:l.cursor], l.runes[l.cursor+1:]...)
}

// deleteWord removes the word before the cursor, along with any spaces
// separating it from the cursor.
func (l *line) deleteWord() {
	end := l.cursor
	for end > 0 && l.runes[end-1] == ' ' {
		end--
	}
	for end > 0 && l.runes[end-1] != ' ' {
		end--
	}
	l.runes = append(l.runes[:end], l.runes[l.cursor:]...)
	l.cursor = end
}

func (l *line) set(text string) {
	l.runes = []rune(text)
	l.cursor = len(l.runes)
}

// readRaw runs the editing loop for one line.
func (e *Editor) readRaw(prompt string) (string, error) {
	var current line
	e.browsing = len(e.history)
	e.stash = ""
	e.render(prompt, current)

	buf := make([]byte, 1)
	pending := make([]byte, 0, 4)
	for {
		n, err := e.in.Read(buf)
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		b := buf[0]

		// Collect the bytes of a multi-byte rune before acting on it.
		if len(pending) > 0 || b >= utf8.RuneSelf {
			pending = append(pending, b)
			if r, size := utf8.DecodeRune(pending); r != utf8.RuneError || size > 1 {
				current.insert(r)
				pending = pending[:0]
				e.render(prompt, current)
			} else if len(pending) >= utf8.UTFMax {
				pending = pending[:0]
			}
			continue
		}

		switch b {
		case keyEnter, '\n':
			io.WriteString(e.out, "\r\n")
			return current.String(), nil
		case keyCtrlC:
			io.WriteString(e.out, "\r\n")
			return "", ErrInterrupted
		case keyCtrlD:
			if len(current.runes) == 0 {
				io.WriteString(e.out, "\r\n")
				return "", io.EOF
			}
			current.deleteForward()
		case keyBackspace, keyDelete:
			current.backspace()
		case keyCtrlA:
			current.cursor = 0
		case keyCtrlE:
			current.cursor = len(current.runes)
		case keyCtrlB:
			if current.cursor > 0 {
				current.cursor--
			}
		case keyCtrlF:
			if current.cursor < len(current.runes) {
				current.cursor++
			}
		case keyCtrlK:
			current.runes = current.runes[:current.cursor]
		case keyCtrlU:
			current.runes = append([]rune(nil), current.runes[current.cursor:]...)
			current.cursor = 0
		case keyCtrlW:
			current.deleteWord()
		case keyCtrlL:
			io.WriteString(e.out, "\x1b[H\x1b[2J")
		case keyCtrlP:
			e.recall(&current, -1)
		case keyCtrlN:
			e.recall(&current, +1)
		case keyTab:
			// Tab inserts two spaces rather than completing: emberdb has
			// no completion, and a silent no-op reads as a broken key.
			current.insert(' ')
			current.insert(' ')
		case keyEscape:
			e.readEscape(&current)
		default:
			if b >= 32 {
				current.insert(rune(b))
			}
		}
		e.render(prompt, current)
	}
}

// readEscape handles the escape sequences the arrow, home and end keys send.
func (e *Editor) readEscape(current *line) {
	buf := make([]byte, 1)
	if _, err := e.in.Read(buf); err != nil || buf[0] != '[' {
		return
	}
	if _, err := e.in.Read(buf); err != nil {
		return
	}
	switch buf[0] {
	case 'A':
		e.recall(current, -1)
	case 'B':
		e.recall(current, +1)
	case 'C':
		if current.cursor < len(current.runes) {
			current.cursor++
		}
	case 'D':
		if current.cursor > 0 {
			current.cursor--
		}
	case 'H':
		current.cursor = 0
	case 'F':
		current.cursor = len(current.runes)
	case '3':
		// Delete arrives as ESC [ 3 ~; swallow the tilde.
		if _, err := e.in.Read(buf); err == nil {
			current.deleteForward()
		}
	}
}

// recall steps through the history. The line being typed is stashed on the
// first step back and restored on stepping past the end, so browsing never
// loses what the user had written.
func (e *Editor) recall(current *line, delta int) {
	if len(e.history) == 0 {
		return
	}
	if e.browsing == len(e.history) {
		if delta > 0 {
			return
		}
		e.stash = current.String()
	}
	next := e.browsing + delta
	switch {
	case next < 0:
		next = 0
	case next > len(e.history):
		next = len(e.history)
	}
	e.browsing = next
	if next == len(e.history) {
		current.set(e.stash)
		return
	}
	current.set(e.history[next])
}

// render redraws the prompt and the line, then parks the cursor where it
// belongs. It rewrites the whole line every keystroke, which at terminal
// speeds is imperceptible and much easier to reason about than tracking what
// changed.
func (e *Editor) render(prompt string, current line) {
	var b strings.Builder
	b.WriteString("\r\x1b[K")
	b.WriteString(prompt)
	b.WriteString(current.String())
	if back := len(current.runes) - current.cursor; back > 0 {
		fmt.Fprintf(&b, "\x1b[%dD", back)
	}
	io.WriteString(e.out, b.String())
}
