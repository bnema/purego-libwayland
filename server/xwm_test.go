package server

import (
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/bnema/purego"
	"golang.org/x/sys/unix"
)

func xwmDisplay(t testing.TB) *Display {
	t.Helper()
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func xwmClient(t testing.TB, d *Display) (Client, int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fds[1]) })
	c, err := d.CreateClient(fds[0])
	if err != nil {
		unix.Close(fds[0])
		t.Fatal(err)
	}
	return c, fds[1]
}

// registry requests exercise libwayland's real advertisement/filter path.
func registry(t testing.TB, d *Display, fd int) int {
	t.Helper()
	request := [12]byte{}
	binary.NativeEndian.PutUint32(request[:4], 1)
	binary.NativeEndian.PutUint32(request[4:8], 12<<16|1)
	binary.NativeEndian.PutUint32(request[8:], 2)
	if _, err := unix.Write(fd, request[:]); err != nil {
		t.Fatal(err)
	}
	if wlEventLoopDispatch(d.loop, 0) < 0 {
		t.Fatal("dispatch failed")
	}
	wlDisplayFlushClients(d.c)
	var buf [4096]byte
	n, err := unix.Read(fd, buf[:])
	if err == unix.EAGAIN {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGlobalFilter(t *testing.T) {
	d := xwmDisplay(t)
	iface := &Interface{Name: "xwm_filter_test", Version: 1}
	if err := NewInterfaces(iface); err != nil {
		t.Fatal(err)
	}
	g, err := d.AddGlobal(iface, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	allowed, a := xwmClient(t, d)
	_, b := xwmClient(t, d)
	calls := 0
	d.SetGlobalFilter(func(c Client, got *Global) bool {
		calls++
		if got != g {
			t.Error("wrong global handle")
		}
		return c == allowed
	})
	if registry(t, d, a) == 0 {
		t.Fatal("allowed client saw no global")
	}
	if registry(t, d, b) != 0 {
		t.Fatal("denied client saw global")
	}
	if calls != 2 {
		t.Fatalf("filter calls = %d", calls)
	}
	allocs := testing.AllocsPerRun(1000, func() { purego.Syscall6(cbGlobalFilter, allowed.c, g.c, d.c, 0, 0, 0) })
	if allocs != 0 {
		t.Fatalf("filter allocs = %g", allocs)
	}
	// Creating a global with registries already present invokes the filter
	// before AddGlobal returns, but still passes the eventual handle.
	var during *Global
	d.SetGlobalFilter(func(_ Client, got *Global) bool { during = got; return false })
	second, err := d.AddGlobal(iface, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if during != second {
		t.Fatal("creation-time filter handle differs")
	}
	d.SetGlobalFilter(nil)
	_, c := xwmClient(t, d)
	if registry(t, d, c) == 0 {
		t.Fatal("nil did not remove filter")
	}
	g.Remove()
	g.Remove()
}

func TestWritableInterest(t *testing.T) {
	d := xwmDisplay(t)
	client, fd := xwmClient(t, d)
	reads, writes := 0, 0
	s, err := d.AddFDRW(fd, func() { reads++ }, func() { writes++ })
	if err != nil {
		t.Fatal(err)
	}
	dispatch := func() {
		if wlEventLoopDispatch(d.loop, 0) < 0 {
			t.Fatal("dispatch failed")
		}
	}
	dispatch()
	if writes != 0 {
		t.Fatal("writable starts enabled")
	}
	if err := s.SetWritable(true); err != nil {
		t.Fatal(err)
	}
	dispatch()
	if writes != 1 || reads != 0 {
		t.Fatalf("reads=%d writes=%d", reads, writes)
	}
	allocs := testing.AllocsPerRun(1000, dispatch)
	if allocs != 0 {
		t.Fatalf("fd dispatch allocs = %g", allocs)
	}
	before := writes
	if err := s.SetWritable(false); err != nil {
		t.Fatal(err)
	}
	dispatch()
	if writes != before {
		t.Fatal("writable not disabled")
	}
	// Readable callbacks also stay allocation-free through the real C loop.
	serverFD, err := client.FD()
	if err != nil {
		t.Fatal(err)
	}
	var payload = [1]byte{1}
	if _, err := unix.Write(serverFD, payload[:]); err != nil {
		t.Fatal(err)
	}
	allocs = testing.AllocsPerRun(1000, dispatch)
	if allocs != 0 {
		t.Fatalf("readable dispatch allocs = %g", allocs)
	}
	if reads == 0 {
		t.Fatal("readable interest lost")
	}
	if _, err := unix.Read(fd, payload[:]); err != nil {
		t.Fatal(err)
	}
	reads = 0
	// Combined masks, hangup routing and removal during readable delivery.
	fdReadable(0, 1|2, s.token)
	if reads != 1 || writes != before+1 {
		t.Fatal("combined mask not delivered")
	}
	s.onReadable = func() { reads++; s.Remove() }
	fdReadable(0, 4|8|2, s.token)
	if reads != 2 || writes != before+1 {
		t.Fatal("removal did not suppress writable")
	}
	if err := s.SetWritable(true); err != nil {
		t.Fatal(err)
	}
	plain, err := d.AddFD(fd, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if plain.SetWritable(true) == nil {
		t.Fatal("enabled without writable callback")
	}
}

func BenchmarkGlobalFilter(b *testing.B) {
	d := xwmDisplay(b)
	iface := &Interface{Name: "xwm_filter_bench", Version: 1}
	if err := NewInterfaces(iface); err != nil {
		b.Fatal(err)
	}
	g, err := d.AddGlobal(iface, 1, nil)
	if err != nil {
		b.Fatal(err)
	}
	c, _ := xwmClient(b, d)
	d.SetGlobalFilter(func(Client, *Global) bool { return true })
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		purego.Syscall6(cbGlobalFilter, c.c, g.c, d.c, 0, 0, 0)
	}
}

func BenchmarkFDCallback(b *testing.B) {
	d := xwmDisplay(b)
	_, fd := xwmClient(b, d)
	s, err := d.AddFDRW(fd, func() {}, func() {})
	if err != nil {
		b.Fatal(err)
	}
	if err := s.SetWritable(true); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		wlEventLoopDispatch(d.loop, 0)
	}
}
