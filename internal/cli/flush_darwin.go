package cli

import "golang.org/x/sys/unix"

// flushInput drops type-ahead the terminal has buffered but nothing has read,
// including a canonical-mode line not yet terminated by enter.
func flushInput(fd int) {
	// FREAD from <sys/fcntl.h>: flush the input queue only.
	const fread = 0x1
	// Best-effort: a failed flush only readmits type-ahead, it must not stop the prompt.
	_ = unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, fread)
}
