package cli

import "golang.org/x/sys/unix"

// flushInput drops type-ahead the terminal has buffered but nothing has read,
// including a canonical-mode line not yet terminated by enter.
func flushInput(fd int) {
	// Best-effort: a failed flush only readmits type-ahead, it must not stop the prompt.
	_ = unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}
