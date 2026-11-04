package server

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"
)

// readDisplayError reads the peer's stream until a wl_display.error event
// and returns its object, code and message. libwayland then destroys the
// client; the hangup follows once the peer sends anything.
func readDisplayError(t *testing.T, fd int) (object, code uint32, msg string) {
	t.Helper()
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 3}); err != nil {
		t.Fatal(err)
	}
	var stream []byte
	buf := make([]byte, 4096)
	for {
		for len(stream) >= 8 {
			size := int(binary.LittleEndian.Uint32(stream[4:]) >> 16)
			if size < 8 || len(stream) < size {
				break
			}
			id, opcode := binary.LittleEndian.Uint32(stream), uint16(binary.LittleEndian.Uint32(stream[4:]))
			body := stream[8:size]
			stream = stream[size:]
			if id != 1 || opcode != 0 {
				continue
			}
			object, code = binary.LittleEndian.Uint32(body), binary.LittleEndian.Uint32(body[4:])
			n := int(binary.LittleEndian.Uint32(body[8:]))
			msg = string(body[12 : 12+n-1])
			// wl_display.sync: the errored client gets no reply but a hangup.
			_, _ = unix.Write(fd, wireMsg(1, 0, u32(2)))
			for {
				r, err := unix.Read(fd, buf)
				if r == 0 || err == unix.ECONNRESET {
					return object, code, msg
				}
				if err != nil {
					t.Fatalf("no hangup after the error: %v", err)
				}
			}
		}
		n, err := unix.Read(fd, buf)
		if err != nil || n == 0 {
			t.Fatalf("read before the error: %d, %v", n, err)
		}
		stream = append(stream, buf[:n]...)
	}
}

func TestPostImplementationError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fallback bool
	}{{"native", false}, {"fallback", true}} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := clientDisplay(t)
			native := wlClientPostImplementationError // set by NewDisplay's load
			if !tc.fallback && native == nil {
				t.Skip("libwayland-server older than 1.21")
			}
			fds := socketPair(t)
			c := adopt(t, d, fds.srv)
			// A format directive must arrive verbatim, not be expanded.
			const msg = "frame too large %s %n 100%"
			ok := false
			if !d.Do(func() {
				if tc.fallback {
					wlClientPostImplementationError = nil
					defer func() { wlClientPostImplementationError = native }()
				}
				ok = c.PostImplementationError(msg)
			}) {
				t.Fatal("display stopped")
			}
			if !ok {
				t.Fatal("PostImplementationError reported no path")
			}
			object, code, got := readDisplayError(t, fds.peer)
			if object != 1 || code != displayErrorImplementation || got != msg {
				t.Fatalf("error object %d code %d %q, want 1 %d %q", object, code, got, displayErrorImplementation, msg)
			}
		})
	}
}

func TestPostImplementationErrorInvalidClient(t *testing.T) {
	if (Client{}).PostImplementationError("x") {
		t.Fatal("zero client reported success")
	}
}
