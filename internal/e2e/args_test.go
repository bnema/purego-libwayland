package e2e_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/bnema/purego-libwayland/protocol/wayland"
	"github.com/bnema/purego-libwayland/server"
	"github.com/bnema/wlturbo"
	"golang.org/x/sys/unix"
)

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
		var zero T
		return zero
	}
}

type argsProxy struct {
	wlturbo.BaseProxy
	received chan argsEvent
	kind     string
}
type argsEvent struct {
	text  string
	data  []byte
	fixed wlturbo.Fixed
	fd    int
}

func (p *argsProxy) Dispatch(e *wlturbo.Event) {
	switch p.kind {
	case "seat":
		p.received <- argsEvent{text: e.String()}
	case "keyboard":
		if e.Opcode == uint16(wayland.KeyboardEventKeymap) {
			_ = e.Uint32()
			p.received <- argsEvent{fd: int(e.Fd())}
		} else if e.Opcode == uint16(wayland.KeyboardEventEnter) {
			_ = e.Uint32()
			_ = e.Uint32()
			p.received <- argsEvent{data: e.Array()}
		}
	case "pointer":
		_ = e.Uint32()
		p.received <- argsEvent{fixed: e.Fixed()}
	}
}

type sourceHandler struct{ requests chan string }

func (h sourceHandler) Offer(_ *wayland.DataSource, mimeType string) { h.requests <- mimeType }
func (sourceHandler) Destroy(*wayland.DataSource)                    {}
func (sourceHandler) SetActions(*wayland.DataSource, uint32)         {}

type managerHandler struct {
	requests chan string
	t        *testing.T
}

func (h managerHandler) CreateDataSource(self *wayland.DataDeviceManager, id uint32) {
	if _, err := wayland.NewDataSource(self.Client(), 1, id, sourceHandler{h.requests}); err != nil {
		h.t.Error(err)
	}
}
func (managerHandler) GetDataDevice(*wayland.DataDeviceManager, uint32, *wayland.Seat) {}
func (managerHandler) Release(*wayland.DataDeviceManager)                              {}

type shmHandler struct {
	requests chan string
	t        *testing.T
}

func (h shmHandler) CreatePool(self *wayland.Shm, id uint32, fd int, size int32) {
	defer unix.Close(fd)
	h.requests <- "pool"
	if _, err := wayland.NewShmPool(self.Client(), 1, id, poolHandler{}); err != nil {
		h.t.Error(err)
	}
}
func (shmHandler) Release(*wayland.Shm) {}

type poolHandler struct{}

func (poolHandler) CreateBuffer(*wayland.ShmPool, uint32, int32, int32, int32, int32, uint32) {}
func (poolHandler) Destroy(*wayland.ShmPool)                                                  {}
func (poolHandler) Resize(self *wayland.ShmPool, size int32)                                  { self.PostError(7, "bad % request") }

type seatHandler struct{ t *testing.T }

type noPointerRequests struct{}

func (noPointerRequests) SetCursor(*wayland.Pointer, uint32, *wayland.Surface, int32, int32) {}
func (noPointerRequests) Release(*wayland.Pointer)                                           {}

type noKeyboardRequests struct{}

func (noKeyboardRequests) Release(*wayland.Keyboard) {}

func (h seatHandler) GetPointer(self *wayland.Seat, id uint32) {
	p, err := wayland.NewPointer(self.Client(), 1, id, noPointerRequests{})
	if err != nil {
		h.t.Error(err)
		return
	}
	p.SendMotion(42, server.FixedFromFloat(-1.5), 0)
}
func (h seatHandler) GetKeyboard(self *wayland.Seat, id uint32) {
	k, err := wayland.NewKeyboard(self.Client(), 1, id, noKeyboardRequests{})
	if err != nil {
		h.t.Error(err)
		return
	}
	fds := []int{0, 0}
	if err := unix.Pipe(fds); err != nil {
		h.t.Error(err)
		return
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	if _, err := unix.Write(fds[1], []byte("fd contents")); err != nil {
		h.t.Error(err)
		return
	}
	k.SendKeymap(1, fds[0], 11)
	k.SendEnter(1, wayland.WrapSurface(self.Resource), []byte{1, 2, 3})
}
func (seatHandler) GetTouch(*wayland.Seat, uint32) {}
func (seatHandler) Release(*wayland.Seat)          {}
func TestArgumentRoundtripAndPostError(t *testing.T) {
	socket, d, _ := startServer(t)
	requests := make(chan string, 1)
	stringsReceived := make(chan string, 1)
	d.Do(func() {
		if err := wayland.NewDataDeviceManagerGlobal(d, 1, func(c server.Client, v, id uint32) {
			if _, e := wayland.NewDataDeviceManager(c, int32(v), id, managerHandler{stringsReceived, t}); e != nil {
				t.Error(e)
			}
		}); err != nil {
			t.Error(err)
		}
		if err := wayland.NewShmGlobal(d, 1, func(c server.Client, v, id uint32) {
			if _, e := wayland.NewShm(c, int32(v), id, shmHandler{requests, t}); e != nil {
				t.Error(e)
			}
		}); err != nil {
			t.Error(err)
		}
		if err := wayland.NewSeatGlobal(d, 1, func(c server.Client, v, id uint32) {
			s, e := wayland.NewSeat(c, int32(v), id, seatHandler{t})
			if e != nil {
				t.Error(e)
				return
			}
			s.SendName("hello")
		}); err != nil {
			t.Error(err)
		}
	})
	c, err := wlturbo.Connect(socket)
	must(t, err)
	defer c.Close()
	must(t, c.Roundtrip())
	bind := func(name string) (uint32, *argsProxy) {
		t.Helper()
		g, ok := c.Registry().FindGlobal(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		id, e := c.Registry().BindID(g.Name, g.Interface, 1)
		must(t, e)
		p := &argsProxy{received: make(chan argsEvent, 4), kind: name[3:]}
		p.SetID(id)
		c.Context().Register(p)
		return id, p
	}
	manager, _ := bind("wl_data_device_manager")
	seat, sp := bind("wl_seat")
	shm, _ := bind("wl_shm")
	source := c.AllocateID()
	must(t, c.SendRequest(manager, uint16(wayland.DataDeviceManagerRequestCreateDataSource), source))
	must(t, c.SendRequest(source, uint16(wayland.DataSourceRequestOffer), "request text"))
	pool := c.AllocateID()
	fds := []int{0, 0}
	must(t, unix.Pipe(fds))
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	must(t, c.SendRequestWithFDs(shm, uint16(wayland.ShmRequestCreatePool), []int{fds[0]}, pool, uint32(0), int32(4096)))
	keyboard := c.AllocateID()
	kp := &argsProxy{received: make(chan argsEvent, 4), kind: "keyboard"}
	kp.SetID(keyboard)
	c.Context().Register(kp)
	pointer := c.AllocateID()
	pp := &argsProxy{received: make(chan argsEvent, 4), kind: "pointer"}
	pp.SetID(pointer)
	c.Context().Register(pp)
	must(t, c.SendRequest(seat, uint16(wayland.SeatRequestGetKeyboard), keyboard))
	must(t, c.SendRequest(seat, uint16(wayland.SeatRequestGetPointer), pointer))
	must(t, c.Roundtrip())
	if text := receive(t, stringsReceived); text != "request text" {
		t.Fatalf("string request = %q", text)
	}
	if receive(t, requests) != "pool" {
		t.Fatal("fd request missing")
	}
	if e := receive(t, sp.received); e.text != "hello" {
		t.Fatalf("string event: %+v", e)
	}
	a, b := receive(t, kp.received), receive(t, kp.received)
	if a.fd <= 0 || !bytes.Equal(b.data, []byte{1, 2, 3}) {
		t.Fatalf("keyboard events: %+v %+v", a, b)
	}
	buf := make([]byte, 32)
	n, e := unix.Read(a.fd, buf)
	unix.Close(a.fd)
	if e != nil || string(buf[:n]) != "fd contents" {
		t.Fatalf("fd: %q %v", buf[:n], e)
	}
	if e := receive(t, pp.received); e.fixed.Float64() != -1.5 {
		t.Fatalf("fixed: %+v", e)
	}
	must(t, c.SendRequest(pool, uint16(wayland.ShmPoolRequestResize), int32(8192)))
	result := make(chan error, 1)
	go func() {
		for i := 0; i < 10; i++ {
			if err := c.Dispatch(); err != nil {
				result <- err
				return
			}
		}
		result <- nil
	}()
	select {
	case err := <-result:
		var displayErr *wlturbo.DisplayError
		if !errors.As(err, &displayErr) || displayErr.ObjectID != pool || displayErr.Code != 7 || displayErr.Message != "bad % request" {
			t.Fatalf("PostError result = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not disconnect after PostError")
	}

}
