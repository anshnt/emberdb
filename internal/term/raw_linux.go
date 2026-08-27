//go:build linux

package term

import "syscall"

// The ioctl requests that read and write terminal attributes on Linux.
const (
	getAttributes = syscall.TCGETS
	setAttributes = syscall.TCSETS
)
