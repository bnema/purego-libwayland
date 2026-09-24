package server

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Handler receives every request sent to a resource.
type Handler func(r *Resource, opcode uint32, args []Arg)

// BindFunc runs when a client binds a global.
type BindFunc func(c Client, version, id uint32)

// Client is a connected wl_client.
type Client struct{ c uintptr }

// Resource is a server-side wl_resource.
type Resource struct {
	c         uintptr
	iface     *Interface
	handler   Handler
	OnDestroy func()
	gone      bool

	// Data holds the owner's state, typically the generated wrapper.
	Data any
}

// live holds Go state reachable from C callbacks. It is only touched on the
// display goroutine, so it needs no lock.
var live = struct {
	resources map[uintptr]*Resource
	binds     map[uintptr]BindFunc
	nextBind  uintptr
}{resources: map[uintptr]*Resource{}, binds: map[uintptr]BindFunc{}}

// Display owns a wl_display. All methods except Do must run on the goroutine
// that calls Run, or before Run starts.
// Do wakes the loop through an eventfd so calls run at once instead of after
// the dispatch timeout.
type Display struct {
	c     uintptr
	loop  uintptr
	calls chan func()
	wake  int
	// wakeSrc owns libwayland's duplicate of wake; only removing the source
	// closes it, destroying the display does not.
	wakeSrc uintptr
	stopped chan struct{}
	state   atomic.Uint32
}

func NewDisplay() (*Display, error) {
	if err := load(); err != nil {
		return nil, err
	}
	c := wlDisplayCreate()
	if c == 0 {
		return nil, errors.New("purego-libwayland: wl_display_create failed")
	}
	wake, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		wlDisplayDestroy(c)
		return nil, fmt.Errorf("purego-libwayland: eventfd: %w", err)
	}
	d := &Display{c: c, loop: wlDisplayGetEventLoop(c), calls: make(chan func(), 64), wake: wake, stopped: make(chan struct{})}
	// The callback only drains the counter; Run then executes queued calls.
	if d.wakeSrc = wlEventLoopAddFD(d.loop, int32(wake), 1 /* WL_EVENT_READABLE */, cbWake, 0); d.wakeSrc == 0 {
		unix.Close(wake)
		wlDisplayDestroy(c)
		return nil, errors.New("purego-libwayland: wl_event_loop_add_fd failed")
	}
	return d, nil
}

// AddSocketFD serves clients on an already bound, listening unix socket.
func (d *Display) AddSocketFD(fd int) error {
	if wlDisplayAddSocketFD(d.c, int32(fd)) != 0 {
		return fmt.Errorf("purego-libwayland: wl_display_add_socket_fd(%d) failed", fd)
	}
	return nil
}

func (d *Display) CreateGlobal(iface *Interface, version int32, bindFn BindFunc) error {
	live.nextBind++
	token := live.nextBind
	live.binds[token] = bindFn
	if wlGlobalCreate(d.c, iface.c, version, token, cbBind) == 0 {
		delete(live.binds, token)
		return fmt.Errorf("purego-libwayland: wl_global_create(%s) failed", iface.Name)
	}
	return nil
}

// Close destroys a display that has not started Run. It is a no-op once Run
// has started; Run owns destruction in that case. Call before Run starts.
func (d *Display) Close() {
	if !d.state.CompareAndSwap(0, 1) {
		return
	}
	d.destroy()
	close(d.stopped)
}

// destroy frees the display and the wake-up fds.
func (d *Display) destroy() {
	wlDisplayDestroyClients(d.c)
	wlEventSourceRemove(d.wakeSrc)
	wlDisplayDestroy(d.c)
	unix.Close(d.wake)
}

// Run dispatches libwayland on one OS-locked goroutine until ctx is done.
func (d *Display) Run(ctx context.Context) error {
	if !d.state.CompareAndSwap(0, 2) {
		return errors.New("purego-libwayland: display already started or closed")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(d.stopped)
	defer d.destroy()
	for {
		if ctx.Err() != nil {
			return nil
		}
		// Run every queued call; each Do also wrote the eventfd.
	drain:
		for {
			select {
			case fn := <-d.calls:
				fn()
			default:
				break drain
			}
		}
		wlDisplayFlushClients(d.c)
		// The timeout only bounds how late ctx cancellation is seen.
		if wlEventLoopDispatch(d.loop, 100) < 0 {
			return errors.New("purego-libwayland: wl_event_loop_dispatch failed")
		}
		wlDisplayFlushClients(d.c)
	}
}

// Do runs fn on the display goroutine and waits for it. It returns false if
// the display loop has stopped (fn was not run, or its run was not confirmed).
// Do must not be called from the display goroutine: it would deadlock.
func (d *Display) Do(fn func()) bool {
	done := make(chan struct{})
	select {
	case d.calls <- func() { fn(); close(done) }:
	case <-d.stopped:
		return false
	}
	var one = [8]byte{1}
	_, _ = unix.Write(d.wake, one[:])
	select {
	case <-done:
		return true
	case <-d.stopped:
		// If both are ready, prefer confirmation of a completed call.
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}

// Stopped is closed after Run returns and the display is destroyed.
func (d *Display) Stopped() <-chan struct{} { return d.stopped }

// LiveResources reports resources not yet destroyed. Display goroutine only.
func LiveResources() int { return len(live.resources) }

func (c Client) CreateResource(iface *Interface, version int32, id uint32, h Handler) (*Resource, error) {
	rc := wlResourceCreate(c.c, iface.c, version, id)
	if rc == 0 {
		return nil, fmt.Errorf("purego-libwayland: wl_resource_create(%s) failed", iface.Name)
	}
	r := &Resource{c: rc, iface: iface, handler: h}
	live.resources[rc] = r
	wlResourceSetDispatcher(rc, cbDispatcher, implMarker(), 0, cbDestroy)
	return r, nil
}

func (r *Resource) Alive() bool { return !r.gone }
func (r *Resource) ID() uint32 {
	if r.gone {
		return 0
	}
	return wlResourceGetID(r.c)
}
func (r *Resource) Version() int32 {
	if r.gone {
		return 0
	}
	return wlResourceGetVersion(r.c)
}
func (r *Resource) Client() Client {
	if r.gone {
		return Client{}
	}
	return Client{wlResourceGetClient(r.c)}
}
func (r *Resource) Iface() *Interface { return r.iface }

// Destroy frees the resource. It is a no-op once the resource is gone, so
// generated destructor requests and handlers may both call it.
func (r *Resource) Destroy() {
	if !r.gone {
		wlResourceDestroy(r.c)
	}
}

// PostEvent sends an event. Arguments must match the event signature; build
// string and array arguments with a Pinner and unpin it after this returns.
// Calling it after destruction is safe and does nothing.
func (r *Resource) PostEvent(opcode uint32, args ...Arg) {
	if r.gone {
		return
	}
	var p unsafe.Pointer
	if len(args) > 0 {
		p = unsafe.Pointer(&args[0])
	}
	var pin runtime.Pinner
	if p != nil {
		pin.Pin(p)
		defer pin.Unpin()
	}
	wlResourcePostEventArr(r.c, opcode, p)
	runtime.KeepAlive(args)
}

// PostError sends a protocol error and disconnects the client.
// Calling it after destruction is safe and does nothing.
func (r *Resource) PostError(code uint32, msg string) {
	if r.gone {
		return
	}
	// wl_resource_post_error takes a printf format; escape it and pass no
	// variadic arguments (purego clears AL, as SysV varargs require).
	b := append([]byte(strings.ReplaceAll(msg, "%", "%%")), 0)
	var pin runtime.Pinner
	pin.Pin(&b[0])
	defer pin.Unpin()
	wlResourcePostError(r.c, code, uintptr(unsafe.Pointer(&b[0])))
	runtime.KeepAlive(b)
}

// implMarker is a non-nil implementation pointer. libwayland only passes it
// back to our dispatcher.
func implMarker() uintptr {
	if marker == 0 {
		tablesMu.Lock()
		if err := tables.reserve(8); err != nil {
			panic(err)
		}
		_, marker = tables.alloc(8)
		tablesMu.Unlock()
	}
	return marker
}

var marker uintptr

// --- C callbacks (display goroutine) ---

func dispatch(_ uintptr, target uintptr, opcode uint32, msg unsafe.Pointer, args unsafe.Pointer) int32 {
	r := live.resources[target]
	sig := msgSignature(msg)
	if r == nil || r.handler == nil {
		closeRequestFDs(sig, args)
		return 0
	}
	n := argCount(sig)
	var a []Arg
	if n > 0 {
		a = append([]Arg(nil), unsafe.Slice((*Arg)(args), n)...)
	}
	r.handler(r, opcode, a)
	return 0
}

func destroyed(res uintptr) {
	r := live.resources[res]
	delete(live.resources, res)
	if r == nil {
		return
	}
	r.gone = true
	if r.OnDestroy != nil {
		r.OnDestroy()
	}
}

// wake drains a Display's eventfd (wl_event_loop_fd_func_t).
func wake(fd int32, mask uint32, data uintptr) int32 {
	var buf [8]byte
	_, _ = unix.Read(int(fd), buf[:])
	return 0
}

func bind(client uintptr, data uintptr, version uint32, id uint32) {
	if fn := live.binds[data]; fn != nil {
		fn(Client{client}, version, id)
	}
}

// msgSignature reads wl_message.signature.
func msgSignature(msg unsafe.Pointer) string {
	p := *(*unsafe.Pointer)(unsafe.Add(msg, 8))
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(p), n))
}

// closeRequestFDs takes ownership of fd arguments when there is no handler.
func closeRequestFDs(sig string, args unsafe.Pointer) {
	i := 0
	for _, ch := range sig {
		if ch == '?' || (ch >= '0' && ch <= '9') {
			continue
		}
		if ch == 'h' {
			_ = unix.Close((*Arg)(unsafe.Add(args, i*8)).Fd())
		}
		i++
	}
}

// argCount counts arguments in a libwayland signature ("?oii", "2n", ...).
func argCount(sig string) int {
	n := 0
	for _, ch := range sig {
		if ch != '?' && (ch < '0' || ch > '9') {
			n++
		}
	}
	return n
}
