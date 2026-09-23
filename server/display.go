package server

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

// Arg is one raw union wl_argument value (int, uint, fixed, object pointer,
// new_id, or fd).
type Arg uint64

func Uint(v uint32) Arg           { return Arg(v) }
func Int(v int32) Arg             { return Arg(uint32(v)) }
func Object(r *Resource) Arg      { return Arg(r.c) }
func (a Arg) Uint() uint32        { return uint32(a) }
func (a Arg) Int() int32          { return int32(uint32(a)) }
func (a Arg) NewID() uint32       { return uint32(a) }
func (a Arg) Resource() *Resource { return live.resources[uintptr(a)] }

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
type Display struct {
	c     uintptr
	loop  uintptr
	calls chan func()
}

func NewDisplay() (*Display, error) {
	if err := load(); err != nil {
		return nil, err
	}
	c := wlDisplayCreate()
	if c == 0 {
		return nil, errors.New("purego-libwayland: wl_display_create failed")
	}
	return &Display{c: c, loop: wlDisplayGetEventLoop(c), calls: make(chan func())}, nil
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

// Run dispatches libwayland on one OS-locked goroutine until ctx is done.
func (d *Display) Run(ctx context.Context) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for {
		select {
		case <-ctx.Done():
			wlDisplayDestroyClients(d.c)
			wlDisplayDestroy(d.c)
			return nil
		case fn := <-d.calls:
			fn()
		default:
		}
		if wlEventLoopDispatch(d.loop, 5) < 0 {
			return errors.New("purego-libwayland: wl_event_loop_dispatch failed")
		}
		wlDisplayFlushClients(d.c)
	}
}

// Do runs fn on the display goroutine and waits for it.
func (d *Display) Do(fn func()) {
	done := make(chan struct{})
	d.calls <- func() { fn(); close(done) }
	<-done
}

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

func (r *Resource) ID() uint32        { return wlResourceGetID(r.c) }
func (r *Resource) Version() int32    { return wlResourceGetVersion(r.c) }
func (r *Resource) Client() Client    { return Client{wlResourceGetClient(r.c)} }
func (r *Resource) Destroy()          { wlResourceDestroy(r.c) }
func (r *Resource) Iface() *Interface { return r.iface }

// PostEvent sends an event. String and array arguments are not supported yet.
func (r *Resource) PostEvent(opcode uint32, args ...Arg) {
	var p unsafe.Pointer
	if len(args) > 0 {
		p = unsafe.Pointer(&args[0])
	}
	wlResourcePostEventArr(r.c, opcode, p)
	runtime.KeepAlive(args)
}

// implMarker is a non-nil implementation pointer. libwayland only passes it
// back to our dispatcher.
func implMarker() uintptr {
	if marker == 0 {
		_, marker = tables.alloc(8)
	}
	return marker
}

var marker uintptr

// --- C callbacks (display goroutine) ---

func dispatch(_ uintptr, target uintptr, opcode uint32, msg unsafe.Pointer, args unsafe.Pointer) int32 {
	r := live.resources[target]
	if r == nil || r.handler == nil {
		return 0
	}
	n := argCount(msgSignature(msg))
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
	if r != nil && r.OnDestroy != nil {
		r.OnDestroy()
	}
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
