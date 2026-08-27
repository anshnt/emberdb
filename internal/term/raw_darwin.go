//go:build darwin

package term

import "syscall"

// The ioctl requests that read and write terminal attributes on macOS.
const (
	getAttributes = syscall.TIOCGETA
	setAttributes = syscall.TIOCSETA
)
