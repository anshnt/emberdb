//go:build linux || darwin

package term

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// IsTerminal reports whether f is a terminal, by asking it for its attributes
// and seeing whether the kernel obliges.
func IsTerminal(f *os.File) bool {
	var attributes syscall.Termios
	return ioctl(f.Fd(), getAttributes, &attributes) == nil
}

// MakeRaw switches a terminal into raw mode and returns a function that puts
// it back. In raw mode the kernel stops buffering lines, echoing characters
// and turning Ctrl-C into a signal, which is what lets the editor interpret
// keys itself.
func MakeRaw(f *os.File) (restore func(), err error) {
	var original syscall.Termios
	if err := ioctl(f.Fd(), getAttributes, &original); err != nil {
		return nil, fmt.Errorf("emberdb: read terminal attributes: %w", err)
	}
	raw := original
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	// Read returns as soon as one byte is available, with no timeout.
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctl(f.Fd(), setAttributes, &raw); err != nil {
		return nil, fmt.Errorf("emberdb: set terminal to raw mode: %w", err)
	}
	return func() {
		// Nothing useful can be done if restoring fails: the process is
		// on its way out and the shell will reset the terminal.
		_ = ioctl(f.Fd(), setAttributes, &original)
	}, nil
}

// ioctl issues a terminal-attribute ioctl.
func ioctl(fd uintptr, request uintptr, attributes *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(attributes)))
	if errno != 0 {
		return errno
	}
	return nil
}
