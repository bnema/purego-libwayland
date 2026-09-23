package server

import (
	"golang.org/x/sys/unix"
	"testing"
	"unsafe"
)

func TestNoHandlerClosesFDs(t *testing.T) {
	for _, sig := range []string{"2?oh", "12uhu"} {
		fds := []int{0, 0}
		if err := unix.Pipe(fds); err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fds[1])
		args := make([]Arg, argCount(sig))
		i := 0
		for _, ch := range sig {
			if ch == '?' || (ch >= '0' && ch <= '9') {
				continue
			}
			if ch == 'h' {
				args[i] = Fd(fds[0])
			}
			i++
		}
		closeRequestFDs(sig, unsafe.Pointer(&args[0]))
		if _, err := unix.FcntlInt(uintptr(fds[0]), unix.F_GETFD, 0); err != unix.EBADF {
			t.Fatalf("signature %s: fd not closed: %v", sig, err)
		}
	}
}
