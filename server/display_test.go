package server

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
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

func TestCloseBeforeRun(t *testing.T) {
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: "\x00purego-close-test"}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.AddSocketFD(fd); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d.Close()
}

// Do must not wait for the dispatch timeout: a compositor sends one call per
// pointer event, at up to 1000 per second.
func TestDoLatency(t *testing.T) {
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { returned <- d.Run(ctx) }()
	defer func() { cancel(); <-returned }()
	start := time.Now()
	for range 1000 {
		if !d.Do(func() {}) {
			t.Fatal("Do failed")
		}
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("1000 calls took %v", elapsed)
	}
}

func TestCloseReleasesFDs(t *testing.T) {
	count := func() int {
		es, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skip(err)
		}
		return len(es)
	}
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	d.Close() // first display loads the library; measure the next ones
	before := count()
	for range 3 {
		d, err := NewDisplay()
		if err != nil {
			t.Fatal(err)
		}
		d.Close()
	}
	if after := count(); after != before {
		t.Fatalf("fd leak: %d -> %d", before, after)
	}
}
