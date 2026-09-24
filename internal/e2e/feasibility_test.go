package e2e_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/purego-libwayland/protocol/wayland"
	"github.com/bnema/purego-libwayland/server"
	"github.com/bnema/wlturbo"
	"golang.org/x/sys/unix"
)

// startServer runs a display with a wl_compositor global on a private socket.
// commits counts wl_surface.commit requests received by Go handlers.
func startServer(t *testing.T, captured ...chan *server.Resource) (socket string, d *server.Display, commits chan uint32) {
	t.Helper()
	d, err := server.NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	socket = filepath.Join(t.TempDir(), "wayland-test")
	// libwayland takes ownership of the listening fd.
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: socket}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		t.Fatal(err)
	}
	if err := d.AddSocketFD(fd); err != nil {
		t.Fatal(err)
	}

	commits = make(chan uint32, 16)
	var serial uint32
	surface := &surfaceHandler{commits: commits, serial: &serial, t: t}
	compositor := &compositorHandler{surface: surface, t: t}
	if len(captured) > 0 {
		compositor.captured = captured[0]
	}
	err = wayland.NewCompositorGlobal(d, 1, func(c server.Client, version, id uint32) {
		if _, err := wayland.NewCompositor(c, int32(version), id, compositor); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("server did not stop")
		}
	})
	return socket, d, commits
}

// Gates 1 and 2: the global exists and wayland-info lists it.
func TestWaylandInfoListsCompositor(t *testing.T) {
	if _, err := exec.LookPath("wayland-info"); err != nil {
		t.Skip("wayland-info not installed")
	}
	socket, _, _ := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wayland-info")
	cmd.Env = []string{"XDG_RUNTIME_DIR=" + filepath.Dir(socket), "WAYLAND_DISPLAY=" + filepath.Base(socket)}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wayland-info: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "wl_compositor") {
		t.Fatalf("wl_compositor missing from wayland-info output:\n%s", out)
	}
}

// Gates 3 and 4: requests reach Go handlers, events reach the client, and a
// disconnect frees every resource. Run repeatedly and under -race.
func TestRequestEventRoundtripAndDisconnect(t *testing.T) {
	socket, d, commits := startServer(t)

	for round := range 20 {
		c, err := wlturbo.Connect(socket)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Roundtrip(); err != nil {
			t.Fatal(err)
		}
		g, ok := c.Registry().FindGlobal("wl_compositor")
		if !ok {
			t.Fatal("wl_compositor not advertised")
		}
		comp, err := c.Registry().BindID(g.Name, g.Interface, 1)
		if err != nil {
			t.Fatal(err)
		}
		surf := c.AllocateID()
		must(t, c.SendRequest(comp, uint16(wayland.CompositorRequestCreateSurface), surf))
		cb := c.AllocateID()
		gotDone := make(chan uint32, 1)
		cbProxy := &doneProxy{done: gotDone}
		cbProxy.SetID(cb)
		c.Context().Register(cbProxy)
		must(t, c.SendRequest(surf, uint16(wayland.SurfaceRequestFrame), cb))
		must(t, c.SendRequest(surf, uint16(wayland.SurfaceRequestCommit)))
		must(t, c.Roundtrip())

		select {
		case id := <-commits:
			if id != surf {
				t.Fatalf("commit on %d, want %d", id, surf)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("commit never reached the Go handler")
		}
		select {
		case serial := <-gotDone:
			if serial != uint32(round+1) {
				t.Fatalf("callback serial %d, want %d", serial, round+1)
			}
		default:
			t.Fatal("wl_callback.done never reached the client")
		}
		_ = c.Close()

		// libwayland frees the client's resources once it notices the hangup.
		deadline := time.Now().Add(2 * time.Second)
		for {
			var n int
			if !d.Do(func() { n = server.LiveResources() }) {
				t.Fatal("display stopped")
			}
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: %d resources still alive after disconnect", round, n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// A resource retained by Go must be harmless after libwayland destroys its client.
func TestPostEventAfterDisconnect(t *testing.T) {
	captured := make(chan *server.Resource, 1)
	socket, d, _ := startServer(t, captured)
	c, err := wlturbo.Connect(socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Roundtrip(); err != nil {
		t.Fatal(err)
	}
	// Create a surface retained by the server after the client disconnects.
	g, ok := c.Registry().FindGlobal("wl_compositor")
	if !ok {
		t.Fatal("wl_compositor not advertised")
	}
	comp, err := c.Registry().BindID(g.Name, g.Interface, 1)
	if err != nil {
		t.Fatal(err)
	}
	surf := c.AllocateID()
	must(t, c.SendRequest(comp, uint16(wayland.CompositorRequestCreateSurface), surf))
	if err := c.Roundtrip(); err != nil {
		t.Fatal(err)
	}
	stale := <-captured
	_ = c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		if !d.Do(func() { n = server.LiveResources() }) {
			t.Fatal("display stopped")
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d resources still alive", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !d.Do(func() {
		if stale == nil {
			t.Error("resource was not created")
			return
		}
		if stale.Alive() {
			t.Error("resource still alive")
		}
		stale.PostEvent(0)
		stale.PostError(0, "gone")
		if stale.ID() != 0 || stale.Version() != 0 || stale.Client() != (server.Client{}) {
			t.Error("destroyed resource returned nonzero metadata")
		}
	}) {
		t.Fatal("display stopped")
	}
}

// doneProxy records wl_callback.done.
type doneProxy struct {
	wlturbo.BaseProxy
	done chan uint32
}

func (p *doneProxy) Dispatch(e *wlturbo.Event) {
	if e.Opcode == uint16(wayland.CallbackEventDone) {
		p.done <- e.Uint32()
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type compositorHandler struct {
	surface  *surfaceHandler
	t        *testing.T
	captured chan *server.Resource
}

func (h *compositorHandler) CreateSurface(self *wayland.Compositor, id uint32) {
	r, err := wayland.NewSurface(self.Client(), self.Version(), id, h.surface)
	if err != nil {
		h.t.Error(err)
		return
	}
	if h.captured != nil {
		h.captured <- r.Resource
	}
}
func (h *compositorHandler) CreateRegion(self *wayland.Compositor, id uint32) {
	if _, err := wayland.NewRegion(self.Client(), self.Version(), id, regionHandler{}); err != nil {
		h.t.Error(err)
	}
}
func (*compositorHandler) Release(*wayland.Compositor) {}

type regionHandler struct{}

func (regionHandler) Destroy(*wayland.Region)                              {}
func (regionHandler) Add(*wayland.Region, int32, int32, int32, int32)      {}
func (regionHandler) Subtract(*wayland.Region, int32, int32, int32, int32) {}

type surfaceHandler struct {
	commits chan uint32
	serial  *uint32
	t       *testing.T
}

func (*surfaceHandler) Destroy(*wayland.Surface)                               {}
func (*surfaceHandler) Attach(*wayland.Surface, *wayland.Buffer, int32, int32) {}
func (*surfaceHandler) Damage(*wayland.Surface, int32, int32, int32, int32)    {}
func (h *surfaceHandler) Frame(self *wayland.Surface, id uint32) {
	cb, err := wayland.NewCallback(self.Client(), 1, id, nil)
	if err != nil {
		h.t.Error(err)
		return
	}
	*h.serial++
	cb.SendDone(*h.serial)
	cb.Destroy()
}
func (*surfaceHandler) SetOpaqueRegion(*wayland.Surface, *wayland.Region)         {}
func (*surfaceHandler) SetInputRegion(*wayland.Surface, *wayland.Region)          {}
func (h *surfaceHandler) Commit(self *wayland.Surface)                            { h.commits <- self.ID() }
func (*surfaceHandler) SetBufferTransform(*wayland.Surface, int32)                {}
func (*surfaceHandler) SetBufferScale(*wayland.Surface, int32)                    {}
func (*surfaceHandler) DamageBuffer(*wayland.Surface, int32, int32, int32, int32) {}
func (*surfaceHandler) Offset(*wayland.Surface, int32, int32)                     {}
func (*surfaceHandler) GetRelease(*wayland.Surface, uint32)                       {}

// The client PID comes from the socket credentials; in-process clients share ours.
func TestClientPID(t *testing.T) {
	captured := make(chan *server.Resource, 1)
	socket, d, _ := startServer(t, captured)
	c, err := wlturbo.Connect(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	must(t, c.Roundtrip())
	g, ok := c.Registry().FindGlobal("wl_compositor")
	if !ok {
		t.Fatal("wl_compositor not advertised")
	}
	comp, err := c.Registry().BindID(g.Name, g.Interface, 1)
	must(t, err)
	must(t, c.SendRequest(comp, uint16(wayland.CompositorRequestCreateSurface), c.AllocateID()))
	must(t, c.Roundtrip())
	r := receive(t, captured)
	pid := make(chan int, 1)
	if !d.Do(func() { pid <- r.Client().PID() }) {
		t.Fatal("display stopped")
	}
	if got := <-pid; got != os.Getpid() {
		t.Fatalf("pid %d, want %d", got, os.Getpid())
	}
}
