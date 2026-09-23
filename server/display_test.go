package server

import (
	"context"
	"golang.org/x/sys/unix"
	"testing"
	"time"
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

func TestDoStopped(t *testing.T) {
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { returned <- d.Run(ctx) }()
	ran := false
	if !d.Do(func() { ran = true }) || !ran {
		t.Fatal("call did not run")
	}
	cancel()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
	select {
	case <-d.Stopped():
	default:
		t.Fatal("Stopped not closed")
	}
	result := make(chan bool, 1)
	go func() { result <- d.Do(func() { t.Error("call ran after stop") }) }()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("call confirmed after stop")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Do blocked after stop")
	}
}

func TestDoBlockedDuringCancellation(t *testing.T) {
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { returned <- d.Run(ctx) }()
	entered := make(chan struct{})
	release := make(chan struct{})
	first := make(chan bool, 1)
	go func() { first <- d.Do(func() { close(entered); <-release }) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first call not started")
	}
	result := make(chan bool, 1)
	go func() { result <- d.Do(func() {}) }()
	cancel()
	close(release)
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("first call blocked")
	}
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run blocked")
	}
	select {
	case <-result:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("blocked Do leaked")
	}
}
