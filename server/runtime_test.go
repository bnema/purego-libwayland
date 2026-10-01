package server

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Tests in this file drive the runtime with hand-built interface tables and a
// raw wire-protocol peer: no client library and no generated bindings.

func i32(v int32) []byte { return u32(uint32(v)) }

func wireArray(b []byte) []byte {
	out := u32(uint32(len(b)))
	out = append(out, b...)
	for len(out)%4 != 0 {
		out = append(out, 0)
	}
	return out
}

// wireEvent is one message read from the server.
type wireEvent struct {
	id     uint32
	opcode uint16
	body   []byte
}

func readFull(t *testing.T, fd, n int) ([]byte, bool) {
	t.Helper()
	buf := make([]byte, n)
	for got := 0; got < n; {
		m, err := unix.Read(fd, buf[got:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if m == 0 {
			if got != 0 {
				t.Fatalf("EOF after %d of %d bytes", got, n)
			}
			return nil, false
		}
		got += m
	}
	return buf, true
}

// readEvent reads one message; ok is false on a clean EOF.
func readEvent(t *testing.T, fd int) (ev wireEvent, ok bool) {
	t.Helper()
	hdr, ok := readFull(t, fd, 8)
	if !ok {
		return ev, false
	}
	size := int(binary.LittleEndian.Uint32(hdr[4:]) >> 16)
	ev = wireEvent{id: binary.LittleEndian.Uint32(hdr), opcode: uint16(binary.LittleEndian.Uint32(hdr[4:]))}
	if size > 8 {
		ev.body, _ = readFull(t, fd, size-8)
	}
	return ev, true
}

// rtEnv is a running display with one adopted client and the raw peer end.
type rtEnv struct {
	d      *Display
	client Client
	peer   int
}

func newRTEnv(t *testing.T) *rtEnv {
	t.Helper()
	d, _ := clientDisplay(t)
	fds := socketPair(t)
	c := adopt(t, d, fds.srv)
	if err := unix.SetsockoptTimeval(fds.peer, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 3}); err != nil {
		t.Fatal(err)
	}
	return &rtEnv{d: d, client: c, peer: fds.peer}
}

func (e *rtEnv) do(t *testing.T, fn func()) {
	t.Helper()
	if !e.d.Do(fn) {
		t.Fatal("display stopped")
	}
}

func (e *rtEnv) send(t *testing.T, msg []byte) {
	t.Helper()
	if _, err := unix.Write(e.peer, msg); err != nil {
		t.Fatal(err)
	}
}

func (e *rtEnv) resource(t *testing.T, iface *Interface, id uint32, h Handler) *Resource {
	t.Helper()
	var r *Resource
	var err error
	e.do(t, func() { r, err = e.client.CreateResource(iface, 3, id, h) })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func rtInterface(t *testing.T, name string) *Interface {
	t.Helper()
	iface := &Interface{
		Name:    name,
		Version: 3,
		Requests: []Message{
			{Name: "all", Signature: "iufsaon"},
			{Name: "nulls", Signature: "?s?o"},
		},
		Events: []Message{
			{Name: "all", Signature: "iufsaon"},
			{Name: "nulls", Signature: "?s?oa"},
		},
	}
	if err := NewInterfaces(iface); err != nil {
		t.Fatal(err)
	}
	return iface
}

type rtRequest struct {
	i      int32
	u      uint32
	f      Fixed
	s      string
	a      []byte
	obj    *Resource
	newID  uint32
	nullS  string
	nullO  *Resource
	objID  uint32
	opcode uint32
}

func TestRuntimeArgumentRoundTrip(t *testing.T) {
	iface := rtInterface(t, "runtime_round_trip_test")
	e := newRTEnv(t)
	// The handler runs on the display goroutine: it reports what it decoded
	// over a channel, which also orders it before the test's reads.
	reqs := make(chan rtRequest, 2)
	var newRes *Resource // written by the handler, read via e.do after the event
	handler := func(r *Resource, opcode uint32, args []Arg) {
		got := rtRequest{opcode: opcode}
		defer func() { reqs <- got }()
		switch opcode {
		case 0:
			got.i, got.u, got.f = args[0].Int(), args[1].Uint(), args[2].Fixed()
			got.s, got.a, got.obj, got.newID = args[3].String(), args[4].Array(), args[5].Resource(), args[6].NewID()
			got.objID = got.obj.ID()
			// Echo everything back as an event, with a server-created resource
			// for the new_id argument.
			var err error
			if newRes, err = r.Client().CreateResource(iface, 1, 0, nil); err != nil {
				t.Error(err)
				return
			}
			var p runtime.Pinner
			defer p.Unpin()
			r.PostEvent(0, Int(got.i), Uint(got.u), FixedArg(FixedFromFloat(got.f.Float())),
				String(&p, got.s, false), Array(&p, got.a), Object(got.obj), NewID(newRes))
		case 1:
			got.nullS, got.nullO = args[0].String(), args[1].Resource()
			var p runtime.Pinner
			defer p.Unpin()
			r.PostEvent(1, String(&p, "", true), Object(nil), Array(&p, nil))
		}
	}
	e.resource(t, iface, 2, handler)

	payload := []byte{1, 2, 3, 4, 5}
	e.send(t, wireMsg(2, 0, i32(-7), u32(0xdeadbeef), i32(-384), wireString("héllo"), wireArray(payload), u32(2), u32(3)))
	ev, ok := readEvent(t, e.peer)
	if !ok {
		t.Fatal("EOF before event")
	}
	got := <-reqs
	if got.i != -7 || got.u != 0xdeadbeef || got.f.Float() != -1.5 || got.s != "héllo" ||
		!bytes.Equal(got.a, payload) || got.obj == nil || got.objID != 2 || got.newID != 3 {
		t.Fatalf("request decoded as %+v", got)
	}
	var newID uint32
	e.do(t, func() { newID = newRes.ID() })
	if newID < 0xff000000 {
		t.Fatalf("server-allocated id %#x", newID)
	}
	want := wireMsg(2, 0, i32(-7), u32(0xdeadbeef), i32(-384), wireString("héllo"), wireArray(payload), u32(2), u32(newID))
	have := wireMsg(ev.id, ev.opcode, ev.body)
	if !bytes.Equal(have, want) {
		t.Fatalf("event bytes\n got %x\nwant %x", have, want)
	}

	// Null string and object in, null string and empty array out.
	e.send(t, wireMsg(2, 1, u32(0), u32(0)))
	ev, ok = readEvent(t, e.peer)
	if !ok {
		t.Fatal("EOF before nulls event")
	}
	got = <-reqs
	if got.opcode != 1 || got.nullS != "" || got.nullO != nil {
		t.Fatalf("null request decoded as %+v", got)
	}
	want = wireMsg(2, 1, u32(0), u32(0), wireArray(nil))
	if have := wireMsg(ev.id, ev.opcode, ev.body); !bytes.Equal(have, want) {
		t.Fatalf("nulls event\n got %x\nwant %x", have, want)
	}
}

func TestResourcePostError(t *testing.T) {
	iface := rtInterface(t, "runtime_post_error_test")
	e := newRTEnv(t)
	r := e.resource(t, iface, 2, nil)
	gone := make(chan struct{})
	e.do(t, func() {
		e.client.OnDestroy(func() { close(gone) })
		r.PostError(7, "100% bad")
	})
	ev, ok := readEvent(t, e.peer)
	if !ok {
		t.Fatal("EOF before wl_display.error")
	}
	want := wireMsg(1, 0, u32(2), u32(7), wireString("100% bad"))
	if have := wireMsg(ev.id, ev.opcode, ev.body); !bytes.Equal(have, want) {
		t.Fatalf("error event\n got %x\nwant %x", have, want)
	}
	// libwayland disconnects the client after a protocol error, once the
	// connection is next serviced: send a wl_display.sync to trigger that.
	e.send(t, wireMsg(1, 0, u32(3)))
	select {
	case <-gone:
	case <-time.After(3 * time.Second):
		t.Fatal("client not disconnected after the protocol error")
	}
	if _, ok := readEvent(t, e.peer); ok {
		t.Fatal("unexpected data after the error")
	}
	e.do(t, func() {
		if r.Alive() {
			t.Error("resource alive after the client was disconnected")
		}
		r.PostError(1, "ignored") // no-op once gone
	})
}

func TestGlobalRemoveEmitsGlobalRemove(t *testing.T) {
	iface := rtInterface(t, "runtime_global_remove_test")
	e := newRTEnv(t)
	var g *Global
	var err error
	e.do(t, func() {
		g, err = e.d.AddGlobal(iface, 3, func(Client, uint32, uint32) {})
	})
	if err != nil {
		t.Fatal(err)
	}
	e.send(t, wireMsg(1, 1, u32(2))) // wl_display.get_registry(new_id 2)
	var name uint32
	for name == 0 {
		ev, ok := readEvent(t, e.peer)
		if !ok {
			t.Fatal("EOF before the global was advertised")
		}
		if ev.id != 2 || ev.opcode != 0 {
			continue
		}
		strLen := int(binary.LittleEndian.Uint32(ev.body[4:]))
		if string(ev.body[8:8+strLen-1]) == iface.Name {
			name = binary.LittleEndian.Uint32(ev.body)
		}
	}
	e.do(t, func() {
		g.Remove()
		g.Remove() // idempotent
		(*Global)(nil).Remove()
	})
	ev, ok := readEvent(t, e.peer)
	if !ok {
		t.Fatal("EOF before global_remove")
	}
	if ev.id != 2 || ev.opcode != 1 || !bytes.Equal(ev.body, u32(name)) {
		t.Fatalf("got %+v, want wl_registry.global_remove(%d)", ev, name)
	}
}

func TestResourceDestroyLifecycle(t *testing.T) {
	iface := rtInterface(t, "runtime_destroy_test")
	e := newRTEnv(t)
	r := e.resource(t, iface, 2, nil)
	destroyed := 0
	e.do(t, func() {
		r.OnDestroy = func() { destroyed++ }
		before := LiveResources()
		if v := r.Version(); v != 3 {
			t.Errorf("Version = %d, want 3", v)
		}
		if r.Iface() != iface || r.ID() != 2 || r.Client() != e.client || !r.Alive() {
			t.Errorf("accessors before destroy: %v %d %v %v", r.Iface(), r.ID(), r.Client(), r.Alive())
		}
		r.Destroy()
		r.Destroy() // no-op once gone
		if n := LiveResources(); n != before-1 {
			t.Errorf("LiveResources %d -> %d, want one fewer", before, n)
		}
		if destroyed != 1 || r.Alive() {
			t.Errorf("destroy callback ran %d times, alive %v", destroyed, r.Alive())
		}
		if r.Version() != 0 || r.ID() != 0 || r.Client() != (Client{}) {
			t.Errorf("accessors after destroy: %d %d %v", r.Version(), r.ID(), r.Client())
		}
		r.PostEvent(0, Int(0), Uint(0), FixedArg(0), Arg(0), Arg(0), Arg(0), Arg(0)) // no-op
	})
	// Destroying a client-allocated id tells the client it can reuse it. A
	// wl_display.sync round trip (callback id 3) ensures the events are flushed.
	e.send(t, wireMsg(1, 0, u32(3)))
	var sawDelete bool
	for done := false; !done; {
		ev, ok := readEvent(t, e.peer)
		if !ok {
			t.Fatal("EOF before the sync callback")
		}
		switch {
		case ev.id == 1 && ev.opcode == 1 && bytes.Equal(ev.body, u32(2)): // wl_display.delete_id(2)
			sawDelete = true
		case ev.id == 3: // wl_callback.done
			done = true
		}
	}
	if !sawDelete {
		t.Fatal("no wl_display.delete_id(2) after Destroy")
	}
}
