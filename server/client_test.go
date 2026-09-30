package server

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// clientDisplay runs a real display and returns it with a stop function that
// waits for Run to return (and so for display destruction).
func clientDisplay(t *testing.T) (*Display, func()) {
	t.Helper()
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	var once bool
	stop := func() {
		if once {
			return
		}
		once = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Run did not return")
		}
	}
	t.Cleanup(stop)
	return d, stop
}

func socketPair(t *testing.T) [2]int {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fds[1]) })
	return [2]int{fds[0], fds[1]}
}

func fdOpen(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

func adopt(t *testing.T, d *Display, fd int) Client {
	t.Helper()
	var c Client
	var err error
	if !d.Do(func() { c, err = d.CreateClient(fd) }) {
		t.Fatal("display stopped")
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientFD(t *testing.T) {
	d, _ := clientDisplay(t)
	fds := socketPair(t)
	c := adopt(t, d, fds[0])
	var (
		got, zero int
		err, zerr error
		allocs    float64
	)
	if !d.Do(func() {
		got, err = c.FD()
		zero, zerr = Client{}.FD()
		allocs = testing.AllocsPerRun(100, func() { _, _ = c.FD() })
	}) {
		t.Fatal("display stopped")
	}
	if err != nil || got != fds[0] {
		t.Fatalf("FD = %d, %v; want %d", got, err, fds[0])
	}
	if zerr == nil || zero != -1 {
		t.Fatalf("invalid client FD = %d, %v", zero, zerr)
	}
	if allocs != 0 {
		t.Fatalf("FD allocs = %g, want 0", allocs)
	}
}

func TestCreateClientFailureKeepsFD(t *testing.T) {
	d, _ := clientDisplay(t)
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(p[0])
	defer unix.Close(p[1])
	var err, errNeg error
	var c, cn Client
	if !d.Do(func() {
		c, err = d.CreateClient(p[0]) // not a socket: SO_PEERCRED fails
		cn, errNeg = d.CreateClient(-1)
	}) {
		t.Fatal("display stopped")
	}
	if err == nil || c != (Client{}) || errNeg == nil || cn != (Client{}) {
		t.Fatalf("got %v %v, %v %v; want errors", c, err, cn, errNeg)
	}
	if !fdOpen(p[0]) {
		t.Fatal("failed CreateClient closed the fd")
	}
}

func TestCreateClientOwnsFD(t *testing.T) {
	d, _ := clientDisplay(t)
	fds := socketPair(t)
	c := adopt(t, d, fds[0])
	gone := make(chan struct{})
	d.Do(func() { c.OnDestroy(func() { close(gone) }) })
	if !fdOpen(fds[0]) {
		t.Fatal("adopted fd closed early")
	}
	_ = unix.Close(fds[1])
	select {
	case <-gone:
	case <-time.After(3 * time.Second):
		t.Fatal("client not destroyed")
	}
	// The listener runs before libwayland closes the fd; Do returns only once
	// the destruction that ran it has finished.
	d.Do(func() {})
	if fdOpen(fds[0]) {
		t.Fatal("libwayland did not close the adopted fd")
	}
}

// A raw client speaks the wire protocol over the other socketpair end.
func wireMsg(id uint32, opcode uint16, args ...[]byte) []byte {
	var body []byte
	for _, a := range args {
		body = append(body, a...)
	}
	b := make([]byte, 8, 8+len(body))
	binary.LittleEndian.PutUint32(b, id)
	binary.LittleEndian.PutUint32(b[4:], uint32(8+len(body))<<16|uint32(opcode))
	return append(b, body...)
}

func u32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func wireString(s string) []byte {
	b := u32(uint32(len(s) + 1))
	b = append(b, s...)
	b = append(b, 0)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func TestCreateClientBindsRegistry(t *testing.T) {
	iface := &Interface{Name: "adopted_client_test", Version: 1}
	if err := NewInterfaces(iface); err != nil {
		t.Fatal(err)
	}
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	bound := make(chan Client, 1)
	if err := d.CreateGlobal(iface, 1, func(c Client, version, id uint32) { bound <- c }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() { cancel(); <-done }()

	fds := socketPair(t)
	c := adopt(t, d, fds[0])
	if err := unix.SetsockoptTimeval(fds[1], unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 3}); err != nil {
		t.Fatal(err)
	}
	// wl_display.get_registry(new_id 2), then wl_display.sync(new_id 3).
	req := append(wireMsg(1, 1, u32(2)), wireMsg(1, 0, u32(3))...)
	if _, err := unix.Write(fds[1], req); err != nil {
		t.Fatal(err)
	}
	// Read until the sync callback's done event (object 3), collecting globals.
	var stream []byte
	var name uint32
	buf := make([]byte, 4096)
	for sawDone := false; !sawDone; {
		n, err := unix.Read(fds[1], buf)
		if err != nil || n == 0 {
			t.Fatalf("read: %d, %v", n, err)
		}
		stream = append(stream, buf[:n]...)
		for len(stream) >= 8 {
			size := int(binary.LittleEndian.Uint32(stream[4:]) >> 16)
			if size < 8 || len(stream) < size {
				break
			}
			id, opcode := binary.LittleEndian.Uint32(stream), uint16(binary.LittleEndian.Uint32(stream[4:]))
			body := stream[8:size]
			if id == 2 && opcode == 0 { // wl_registry.global(name, interface, version)
				strLen := int(binary.LittleEndian.Uint32(body[4:]))
				if string(body[8:8+strLen-1]) == iface.Name {
					name = binary.LittleEndian.Uint32(body)
				}
			}
			if id == 3 {
				sawDone = true
			}
			stream = stream[size:]
		}
	}
	if name == 0 {
		t.Fatal("global not advertised to the adopted client")
	}
	// wl_registry.bind(name, interface, version, new_id 4)
	if _, err := unix.Write(fds[1], wireMsg(2, 0, u32(name), wireString(iface.Name), u32(1), u32(4))); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-bound:
		if got != c {
			t.Fatalf("bind client %v, want adopted %v", got, c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bind never reached the global")
	}
}

func TestOnDestroyOnDisconnect(t *testing.T) {
	iface := &Interface{Name: "on_destroy_test", Version: 1}
	if err := NewInterfaces(iface); err != nil {
		t.Fatal(err)
	}
	d, _ := clientDisplay(t)
	fds := socketPair(t)
	c := adopt(t, d, fds[0])
	var (
		order              []int
		resAlive, fdOK     bool
		other              = map[Client]int{c: 1}
		freeBefore, free   int
		fired              = make(chan struct{})
		res                *Resource
		createErr, fdError error
	)
	if !d.Do(func() {
		res, createErr = c.CreateResource(iface, 1, 2, nil)
		for i := 1; i <= 3; i++ {
			c.OnDestroy(func() {
				order = append(order, i)
				if i == 3 {
					var fd int
					fd, fdError = c.FD()
					fdOK = fd == fds[0]
					resAlive = res.Alive()
					delete(other, c)
					close(fired)
				}
			})
		}
		freeBefore = len(listeners.free) // after the slots are taken
		c.OnDestroy(nil)
		Client{}.OnDestroy(func() { t.Error("zero client listener ran") })
	}) {
		t.Fatal("display stopped")
	}
	if createErr != nil {
		t.Fatal(createErr)
	}
	select {
	case <-fired:
		t.Fatal("fired before disconnect")
	case <-time.After(50 * time.Millisecond):
	}
	_ = unix.Close(fds[1])
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("OnDestroy did not fire on disconnect")
	}
	d.Do(func() { free = len(listeners.free) })
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("order %v, want [1 2 3]", order)
	}
	if fdError != nil || !fdOK || !resAlive {
		t.Fatalf("during OnDestroy: fd ok=%v err=%v resource alive=%v", fdOK, fdError, resAlive)
	}
	if len(other) != 0 || res.Alive() {
		t.Fatalf("map %v, resource alive %v", other, res.Alive())
	}
	if free != freeBefore+3 || len(live.clientGone) != 0 {
		t.Fatalf("listener slots not recycled: free %d -> %d, live %d", freeBefore, free, len(live.clientGone))
	}
}

func TestOnDestroyOnDisplayClose(t *testing.T) {
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	fds := socketPair(t)
	c, err := d.CreateClient(fds[0])
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	c.OnDestroy(func() { n++ })
	c.OnDestroy(func() { n += 10 })
	d.Close()
	d.Close()
	if n != 11 {
		t.Fatalf("listeners ran %d, want 11 (each once)", n)
	}
}

func TestOnDestroyOnRunStop(t *testing.T) {
	d, stop := clientDisplay(t)
	fds := socketPair(t)
	c := adopt(t, d, fds[0])
	n := 0
	d.Do(func() { c.OnDestroy(func() { n++ }) })
	stop()
	if n != 1 {
		t.Fatalf("listener ran %d times, want 1", n)
	}
}

// A client destroyed at disconnect must not fire again when the display stops.
func TestOnDestroyOnce(t *testing.T) {
	d, stop := clientDisplay(t)
	fds := socketPair(t)
	c := adopt(t, d, fds[0])
	fired := make(chan struct{}, 4)
	d.Do(func() { c.OnDestroy(func() { fired <- struct{}{} }) })
	_ = unix.Close(fds[1])
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("OnDestroy did not fire")
	}
	stop()
	if len(fired) != 0 {
		t.Fatal("OnDestroy fired twice")
	}
}
