package server

import (
	"context"
	"os"
	"testing"
	"unsafe"

	"github.com/bnema/purego"
	"golang.org/x/sys/unix"
)

// hotResource creates a real wl_client/resource on the display thread without
// needing a separate client event loop.
func hotResource(t testing.TB, d *Display, iface *Interface, h Handler) *Resource {
	t.Helper()
	lib, err := purego.Dlopen("libwayland-server.so.0", purego.RTLD_NOW)
	if err != nil {
		t.Fatal(err)
	}
	create, err := purego.Dlsym(lib, "wl_client_create")
	if err != nil {
		t.Fatal(err)
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fds[1]) })
	var r *Resource
	if !d.Do(func() {
		client, _, _ := purego.SyscallN(create, d.c, uintptr(fds[0]))
		if client == 0 {
			_ = unix.Close(fds[0])
			return
		}
		r, err = (Client{client}).CreateResource(iface, 1, 2, h)
	}) {
		t.Fatal("display stopped")
	}
	if err != nil || r == nil {
		t.Fatalf("create resource: %v", err)
	}
	return r
}

func hotDisplay(t testing.TB, iface *Interface, h Handler) (*Display, *Resource) {
	t.Helper()
	if err := NewInterfaces(iface); err != nil {
		t.Fatal(err)
	}
	d, err := NewDisplay()
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
	return d, hotResource(t, d, iface, h)
}

func TestServerAllocatedResourceID(t *testing.T) {
	iface := &Interface{Name: "server_allocated_id_test", Version: 1}
	d, existing := hotDisplay(t, iface, nil)
	if !d.Do(func() {
		r, err := (Client{existing.client}).CreateResource(iface, 1, 0, nil)
		if err != nil {
			t.Errorf("create server-side resource: %v", err)
			return
		}
		if r.ID() == 0 || r.ID() != wlResourceGetID(r.c) {
			t.Errorf("resource ID = %d, libwayland ID = %d", r.ID(), wlResourceGetID(r.c))
		}
	}) {
		t.Fatal("display stopped")
	}
}

func requestMessage(iface *Interface) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Add(unsafe.Pointer(&tables.mem[0]), iface.c-tables.base+16))
}

var hotSink uint32

func hotHandler(_ *Resource, _ uint32, args []Arg) { hotSink = args[0].Uint() }

func TestHotPathAllocs(t *testing.T) {
	iface := &Interface{Name: "hot_path_test", Version: 1, Requests: []Message{{Name: "request", Signature: "uuu"}}, Events: []Message{{Name: "event", Signature: "uuu"}}}
	d, r := hotDisplay(t, iface, hotHandler)
	var post, dispatchAllocs float64
	if !d.Do(func() {
		post = testing.AllocsPerRun(100, func() { r.PostEvent(0, Uint(1), Uint(2), Uint(3)) })
		a := [3]Arg{Uint(1), Uint(2), Uint(3)}
		dispatchAllocs = testing.AllocsPerRun(100, func() { dispatch(0, r.c, 0, requestMessage(iface), unsafe.Pointer(&a[0])) })
	}) {
		t.Fatal("display stopped")
	}
	if post != 0 || dispatchAllocs != 0 {
		t.Fatalf("allocs: PostEvent=%g dispatch=%g, want zero", post, dispatchAllocs)
	}
}

func BenchmarkDispatch(b *testing.B) {
	iface := &Interface{Name: "hot_dispatch_bench", Version: 1, Requests: []Message{{Name: "request", Signature: "uuu"}}}
	d, r := hotDisplay(b, iface, hotHandler)
	a := [3]Arg{Uint(1), Uint(2), Uint(3)}
	if !d.Do(func() {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			dispatch(0, r.c, 0, requestMessage(iface), unsafe.Pointer(&a[0]))
		}
		b.StopTimer()
	}) {
		b.Fatal("display stopped")
	}
}
func BenchmarkPostEvent(b *testing.B) {
	iface := &Interface{Name: "hot_event_bench", Version: 1, Events: []Message{{Name: "event", Signature: "uuu"}}}
	d, r := hotDisplay(b, iface, nil)
	if !d.Do(func() {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			r.PostEvent(0, Uint(1), Uint(2), Uint(3))
		}
		b.StopTimer()
	}) {
		b.Fatal("display stopped")
	}
}

func TestClientCredentials(t *testing.T) {
	iface := &Interface{Name: "cred_test", Version: 1, Requests: []Message{{Name: "request", Signature: "uuu"}}, Events: []Message{{Name: "event", Signature: "uuu"}}}
	d, r := hotDisplay(t, iface, hotHandler)
	var (
		got, zero Credentials
		err, zerr error
		allocs    float64
		pid       int
	)
	c := r.Client()
	if !d.Do(func() {
		got, err = c.Credentials()
		zero, zerr = Client{}.Credentials()
		pid = c.PID()
		allocs = testing.AllocsPerRun(100, func() { _, _ = c.Credentials() })
	}) {
		t.Fatal("display stopped")
	}
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != os.Getpid() || got.UID != uint32(os.Getuid()) || got.GID != uint32(os.Getgid()) || pid != got.PID {
		t.Fatalf("credentials %+v pid %d, want pid=%d uid=%d gid=%d", got, pid, os.Getpid(), os.Getuid(), os.Getgid())
	}
	if zerr == nil || zero != (Credentials{}) || (Client{}).PID() != 0 {
		t.Fatalf("invalid client: %+v, %v", zero, zerr)
	}
	if allocs != 0 {
		t.Fatalf("Credentials allocs = %g, want 0", allocs)
	}
}
