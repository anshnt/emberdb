//go:build !linux && !darwin

package term

import (
	"errors"
	"os"
)

// IsTerminal reports whether f is a terminal. On platforms emberdb has no raw
// mode for it always answers no, which makes the editor fall back to reading
// whole lines. That loses history and in-line editing, not correctness.
func IsTerminal(f *os.File) bool { return false }

// MakeRaw is not implemented on this platform.
func MakeRaw(f *os.File) (func(), error) {
	return nil, errors.New("emberdb: raw terminal mode is not supported on this platform")
}
