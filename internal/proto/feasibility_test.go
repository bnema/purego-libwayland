package proto_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/purego-libwayland/internal/proto"
	"github.com/bnema/purego-libwayland/server"
	"github.com/bnema/wlturbo"
	"golang.org/x/sys/unix"
)

// startServer runs a display with a wl_compositor global on a private socket.
// commits counts wl_surface.commit requests received by Go handlers.
func startServer(t *testing.T) (socket string, d *server.Display, commits chan uint32) {
	t.Helper()
	if err := proto.Init(); err != nil {
		t.Fatal(err)
	}
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
	surface := func(r *server.Resource, op uint32, args []server.Arg) {
		switch op {
		case proto.SurfaceDestroy:
			r.Destroy()
		case proto.SurfaceFrame:
			cb, err := r.Client().CreateResource(proto.Callback, 1, args[0].NewID(), nil)
			if err != nil {
				t.Error(err)
				return
			}
			serial++
			cb.PostEvent(proto.CallbackDone, server.Uint(serial))
			cb.Destroy()
		case proto.SurfaceCommit:
			commits <- r.ID()
		}
	}
	compositor := func(r *server.Resource, op uint32, args []server.Arg) {
		iface, h := proto.Surface, server.Handler(surface)
		if op == proto.CompositorCreateRegion {
			iface, h = proto.Region, func(r *server.Resource, op uint32, _ []server.Arg) {
				if op == proto.RegionDestroy {
					r.Destroy()
				}
			}
		}
		if _, err := r.Client().CreateResource(iface, r.Version(), args[0].NewID(), h); err != nil {
			t.Error(err)
		}
	}
	err = d.CreateGlobal(proto.Compositor, 1, func(c server.Client, version, id uint32) {
		if _, err := c.CreateResource(proto.Compositor, int32(version), id, compositor); err != nil {
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
		if err := <-done; err != nil {
			t.Error(err)
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
		must(t, c.SendRequest(comp, proto.CompositorCreateSurface, surf))
		cb := c.AllocateID()
		gotDone := make(chan uint32, 1)
		cbProxy := &doneProxy{done: gotDone}
		cbProxy.SetID(cb)
		c.Context().Register(cbProxy)
		must(t, c.SendRequest(surf, proto.SurfaceFrame, cb))
		must(t, c.SendRequest(surf, proto.SurfaceCommit))
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
			d.Do(func() { n = server.LiveResources() })
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

// doneProxy records wl_callback.done.
type doneProxy struct {
	wlturbo.BaseProxy
	done chan uint32
}

func (p *doneProxy) Dispatch(e *wlturbo.Event) {
	if e.Opcode == proto.CallbackDone {
		p.done <- e.Uint32()
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
