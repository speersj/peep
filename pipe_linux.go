package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// setPipeSize asks the kernel for a larger pipe buffer. It is best effort:
// on failure the default size is kept.
func setPipeSize(f *os.File, size int) {
	rc, err := f.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		_, _ = unix.FcntlInt(fd, unix.F_SETPIPE_SZ, size)
	})
}
