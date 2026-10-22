package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/bnema/purego"
	"golang.org/x/sys/unix"
)

// Handler receives every request sent to a resource. args borrows libwayland's
// argument array and must not be retained after the handler returns.
type Handler func(r *Resource, opcode uint32, args []Arg)

// BindFunc runs when a client binds a global.
type BindFunc func(c Client, version, id uint32)

// Client is a connected wl_client. It is a comparable handle to the C object,
// usable as a map key until the client is destroyed (see Client.OnDestroy).
type Client struct{ c uintptr }

// Resource is a server-side wl_resource.
type Resource struct {
	c         uintptr
	id        uint32
	version   int32
	client    uintptr
	iface     *Interface
	handler   Handler
	OnDestroy func()
	gone      bool
	bare      bool // created by libwayland, no destroy callback

	// Data holds the owner's state, typically the generated wrapper.
	Data any
}

// live holds Go state reachable from C callbacks. It is only touched on the
// display goroutine, so it needs no lock.
var live = struct {
	resources map[uintptr]*Resource
	binds     map[uintptr]BindFunc
	nextBind  uintptr
	// clientGone maps wl_listener addresses to OnDestroy functions.
	clientGone map[uintptr]func()
	// fds maps AddFD/AddFDRW tokens to their sources.
	fds      map[uintptr]*Source
	nextFD   uintptr
	globals  map[uintptr]*Global
	displays map[uintptr]*Display
}{resources: map[uintptr]*Resource{}, binds: map[uintptr]BindFunc{}, clientGone: map[uintptr]func(){}, fds: map[uintptr]*Source{}, globals: map[uintptr]*Global{}, displays: map[uintptr]*Display{}}

// Display owns a wl_display. All methods except Do and Post must run on the
// goroutine that calls Run, or before Run starts.
//
// Resource, bind and client-listener state is shared by the whole process, so
// only one Display may be running at a time per process.
//
// Do and Post wake the loop through an eventfd so calls run at once instead of
// after the dispatch timeout.
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

	// postMu guards posts and closed. spare is only used by the display
	// goroutine: it is the emptied batch swapped back in on the next drain, so
	// steady-state Post does not allocate.
	postMu sync.Mutex
	posts  []func()
	spare  []func()
	// armed is true while an eventfd wake for posts is pending, so a burst of
	// Posts writes the eventfd once.
	armed  atomic.Bool
	closed atomic.Bool // set under postMu before the wake fd is closed
	// wakeWrites counts eventfd writes made for posts (diagnostics and tests).
	wakeWrites atomic.Uint64

	// sources are the AddFD sources not yet removed (display goroutine only).
	sources       map[*Source]struct{}
	filter        func(Client, *Global) bool
	pendingGlobal *Global
}

// maxDrainRounds bounds how many times one loop iteration re-drains calls and
// posts that were queued by callbacks, so a self-posting callback cannot starve
// libwayland's dispatch and client flushing.
const maxDrainRounds = 8

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
	d := &Display{c: c, loop: wlDisplayGetEventLoop(c), calls: make(chan func(), 64), wake: wake, stopped: make(chan struct{}), sources: map[*Source]struct{}{}}
	// The callback only drains the counter; Run then executes queued calls.
	if d.wakeSrc = wlEventLoopAddFD(d.loop, int32(wake), 1 /* WL_EVENT_READABLE */, cbWake, 0); d.wakeSrc == 0 {
		unix.Close(wake)
		wlDisplayDestroy(c)
		return nil, errors.New("purego-libwayland: wl_event_loop_add_fd failed")
	}
	live.displays[c] = d
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
	_, err := d.AddGlobal(iface, version, bindFn)
	return err
}

// Global is a global that can be withdrawn, such as a wl_output on hotplug.
type Global struct {
	c uintptr
	d *Display
}

// SetGlobalFilter controls global visibility and binding for each client. nil
// removes the filter. Install it before advertising restricted globals; it does
// not retract announcements already sent. The callback must not block or create
// globals. Display goroutine only.
func (d *Display) SetGlobalFilter(fn func(Client, *Global) bool) {
	d.filter = fn
	callback := uintptr(0)
	if fn != nil {
		callback = cbGlobalFilter
	}
	wlDisplaySetGlobalFilter(d.c, callback, d.c)
}

func globalFilter(client, global, data uintptr) uint32 {
	d := live.displays[data]
	if d == nil || d.filter == nil {
		return 1
	}
	g := live.globals[global]
	// wl_global_create announces synchronously, before returning its pointer.
	if g == nil {
		g = d.pendingGlobal
	}
	if g != nil && d.filter(Client{client}, g) {
		return 1
	}
	return 0
}

// AddGlobal advertises a global and returns a handle to remove it later.
func (d *Display) AddGlobal(iface *Interface, version int32, bindFn BindFunc) (*Global, error) {
	live.nextBind++
	token := live.nextBind
	live.binds[token] = bindFn
	g := &Global{d: d}
	d.pendingGlobal = g
	c := wlGlobalCreate(d.c, iface.c, version, token, cbBind)
	d.pendingGlobal = nil
	if c == 0 {
		delete(live.binds, token)
		return nil, fmt.Errorf("purego-libwayland: wl_global_create(%s) failed", iface.Name)
	}
	g.c = c
	live.globals[c] = g
	return g, nil
}

// Remove withdraws the global: clients get wl_registry.global_remove.
// Existing resources stay valid. A client that binds before it sees the
// removal still gets its bind function called, so it always receives a
// resource (the protocol allows binding a just-removed global); the bind
// function should then send inert state. The global is freed with the
// display. Display goroutine only; idempotent.
func (g *Global) Remove() {
	if g == nil || g.c == 0 {
		return
	}
	wlGlobalRemove(g.c)
	g.c = 0
}

// AckGlobalRemove implements wl_fixes.ack_global_remove: libwayland checks
// that registry announced the removed global name, posting
// wl_fixes.invalid_ack_remove on fixes otherwise. It reports false when
// libwayland-server is older than 1.26 and has no support.
// Display goroutine only.
func AckGlobalRemove(fixes, registry *Resource, name uint32) bool {
	if symFixesAckGlobalRemove == 0 {
		return false
	}
	if fixes == nil || fixes.gone || registry == nil || registry.gone {
		return true
	}
	wlFixesHandleAckGlobalRemove(fixes.c, registry.c, name)
	return true
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
	// Stop accepting posts before the wake fd is closed (its number could be
	// reused), and drop what never ran.
	d.postMu.Lock()
	d.closed.Store(true)
	clear(d.posts)
	d.posts, d.spare = nil, nil
	d.postMu.Unlock()
	for s := range d.sources {
		s.remove()
	}
	clear(d.sources)
	wlDisplayDestroyClients(d.c)
	wlEventSourceRemove(d.wakeSrc)
	wlDisplayDestroy(d.c)
	delete(live.displays, d.c)
	for p, g := range live.globals {
		if g.d == d {
			g.c = 0
			delete(live.globals, p)
		}
	}
	d.filter = nil
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
	// Wait for the event loop's epoll fd in Go's netpoller instead of blocking
	// in epoll_wait inside C: the goroutine parks, its P is released, and the
	// runtime's sysmon can sleep deeply while the server is idle. libwayland
	// is then dispatched without blocking.
	loopFile, err := d.openLoopFile()
	if err != nil {
		return err
	}
	defer loopFile.Close() // runs before destroy: the dup must not outlive the epoll fd
	rc, err := loopFile.SyscallConn()
	if err != nil {
		return fmt.Errorf("purego-libwayland: event loop fd: %w", err)
	}
	// Cancellation wakes the loop through the eventfd.
	stop := context.AfterFunc(ctx, d.wakeGuarded)
	defer stop()
	ready := func(fd uintptr) bool {
		// Level-triggered check: the netpoller is edge-triggered, and a
		// dispatch can leave events (libwayland takes 32 per epoll_wait).
		fds := [1]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds[:], 0)
		return n > 0 || (err != nil && err != unix.EINTR)
	}
	consecutiveFailures := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		// Run every queued post and call. Wakes are coalesced, so the eventfd
		// is not written once per item; it only makes the wait below
		// return at once when work arrives while the loop is asleep.
		d.drain()
		wlDisplayFlushClients(d.c)
		if err := rc.Read(ready); err != nil {
			return fmt.Errorf("purego-libwayland: wait for event loop: %w", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if wlEventLoopDispatch(d.loop, 0) < 0 {
			errno := lastErrno()
			consecutiveFailures++
			if consecutiveFailures < 16 {
				continue
			}
			return fmt.Errorf("purego-libwayland: wl_event_loop_dispatch failed %d times in a row: %w", consecutiveFailures, errno)
		}
		consecutiveFailures = 0
		wlDisplayFlushClients(d.c)
	}
}

// openLoopFile wraps a duplicate of the event loop's epoll fd for the netpoller.
func (d *Display) openLoopFile() (*os.File, error) {
	fd := int(wlEventLoopGetFD(d.loop))
	if fd < 0 {
		return nil, errors.New("purego-libwayland: wl_event_loop_get_fd failed")
	}
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("purego-libwayland: dup event loop fd: %w", err)
	}
	if err := unix.SetNonblock(dup, true); err != nil {
		unix.Close(dup)
		return nil, fmt.Errorf("purego-libwayland: nonblock event loop fd: %w", err)
	}
	return os.NewFile(uintptr(dup), "wl-event-loop"), nil
}

// drain runs queued posts and Do calls until none are left, at most
// maxDrainRounds rounds. Posts are taken again after each call is received and
// before it runs, so a Post that happened before a Do always runs before that
// Do's function. When the cap is hit the loop is woken again so the next
// dispatch returns at once.
func (d *Display) drain() {
	for range maxDrainRounds {
		ran := d.runPosts()
	calls:
		for {
			select {
			case fn := <-d.calls:
				d.runPosts()
				fn()
				ran = true
			default:
				break calls
			}
		}
		if !ran {
			return
		}
	}
	d.armed.Store(true)
	d.wakeLoop()
}

// runPosts runs the posts queued so far and reports whether there were any.
func (d *Display) runPosts() bool {
	// Clear armed before taking the batch: a Post that lands after the swap
	// sees armed false and writes the eventfd again.
	d.armed.Store(false)
	d.postMu.Lock()
	batch := d.posts
	d.posts, d.spare = d.spare, nil
	d.postMu.Unlock()
	for _, fn := range batch {
		fn()
	}
	clear(batch)
	d.spare = batch[:0]
	return len(batch) > 0
}

// wakeOne is the eventfd increment. It is read-only, so sharing it between
// goroutines is safe and avoids a per-call buffer (which escapes under -race).
var wakeOne = [8]byte{1}

// wakeLoop writes the eventfd on behalf of posts. The caller must hold postMu
// (or be the display goroutine, which destroy cannot overlap) so the fd cannot
// be closed, and its number reused, underneath the write.
func (d *Display) wakeLoop() {
	d.wakeWrites.Add(1)
	_, _ = unix.Write(d.wake, wakeOne[:])
}

// wakeGuarded wakes the loop from any goroutine: it takes postMu and skips the
// write once the display is closed, so a late caller never writes to a reused
// fd number.
func (d *Display) wakeGuarded() {
	d.postMu.Lock()
	defer d.postMu.Unlock()
	if d.closed.Load() {
		return
	}
	_, _ = unix.Write(d.wake, wakeOne[:])
}

// Post queues fn to run on the display goroutine and returns at once. Unlike
// Do it never waits, and unlike Do it may be called from any goroutine,
// including the display goroutine itself.
//
// Posts run in the order they were made, and a Post made before a Do (by the
// same goroutine, or otherwise known to happen before it) runs before that Do's
// function. A Post made by a posted function or a Do function runs in a later
// drain round of the same loop iteration (at most 8 rounds; past that the loop
// is woken again and continues after one libwayland dispatch). Wakes are coalesced:
// any number of Posts while one wake is pending write the eventfd once.
//
// fn must not block: it runs on the goroutine that serves every client. After
// the display has stopped or been closed, Post silently drops fn, and posts
// that had not run when the display stopped are dropped too. In steady state
// Post does not allocate.
//
// The queue is unbounded: Post applies no backpressure, so a producer that
// outpaces the display goroutine grows memory without limit. Callers that can
// produce faster than the loop runs must bound or coalesce their own posts.
func (d *Display) Post(fn func()) {
	if fn == nil {
		return
	}
	d.postMu.Lock()
	defer d.postMu.Unlock()
	if d.closed.Load() {
		return
	}
	d.posts = append(d.posts, fn)
	// Written under postMu so destroy cannot close the fd underneath.
	if d.armed.CompareAndSwap(false, true) {
		d.wakeLoop()
	}
}

// Source is an fd registered with the display's event loop by AddFD.
type Source struct {
	d                      *Display
	c                      uintptr
	token                  uintptr
	onReadable, onWritable func()
}

// AddFD calls onReadable on the display goroutine whenever fd is readable. The
// registration is level-triggered: onReadable must consume the readiness (read
// the eventfd or timerfd, accept, ...) or it is called again at once. It
// should not block.
//
// onReadable is also called when the fd hangs up or errors (EPOLLHUP,
// EPOLLERR), which level-triggered polling keeps reporting. The callback should
// read the fd, and on EOF or error call Remove (it may do so from within the
// callback), or it is called again at once.
//
// libwayland duplicates fd; the caller still owns and must close its own
// descriptor, and should do so after Remove. Call AddFD only on the display
// goroutine or before Run starts. Sources not removed are released when the
// display is destroyed.
func (d *Display) AddFD(fd int, onReadable func()) (*Source, error) {
	return d.AddFDRW(fd, onReadable, nil)
}

// AddFDRW is AddFD with an optional writable callback. Writable interest starts
// disabled; use SetWritable to arm it after a non-blocking write returns EAGAIN.
// Both callbacks must not block. Readable runs first; removing the source there
// suppresses writable delivery in the same dispatch.
func (d *Display) AddFDRW(fd int, onReadable, onWritable func()) (*Source, error) {
	if fd < 0 {
		return nil, fmt.Errorf("purego-libwayland: AddFD: invalid fd %d", fd)
	}
	if onReadable == nil {
		return nil, errors.New("purego-libwayland: AddFD: nil callback")
	}
	if d.closed.Load() {
		return nil, errors.New("purego-libwayland: AddFD: display closed")
	}
	live.nextFD++
	token := live.nextFD
	s := &Source{d: d, token: token, onReadable: onReadable, onWritable: onWritable}
	live.fds[token] = s
	c := wlEventLoopAddFD(d.loop, int32(fd), 1 /* WL_EVENT_READABLE */, cbFD, token)
	if c == 0 {
		delete(live.fds, token)
		return nil, fmt.Errorf("purego-libwayland: wl_event_loop_add_fd(%d) failed", fd)
	}
	s.c = c
	d.sources[s] = struct{}{}
	return s, nil
}

// SetWritable toggles writable interest while always retaining readable interest.
// Disable it when buffered writes are drained to avoid a busy loop. Display
// goroutine only. A removed source is a no-op; enabling requires onWritable.
func (s *Source) SetWritable(on bool) error {
	if s == nil || s.c == 0 {
		return nil
	}
	mask := uint32(1)
	if on {
		if s.onWritable == nil {
			return errors.New("purego-libwayland: no writable callback")
		}
		mask |= 2
	}
	if wlEventSourceFDUpdate(s.c, mask) < 0 {
		return lastErrno()
	}
	return nil
}

// Remove stops the callbacks and releases libwayland's duplicate of the fd.
// It may be called from the source's own callback. Display goroutine only;
// idempotent, and a no-op after the display was destroyed.
func (s *Source) Remove() {
	if s == nil || s.c == 0 {
		return
	}
	delete(s.d.sources, s)
	s.remove()
}

func (s *Source) remove() {
	delete(live.fds, s.token)
	wlEventSourceRemove(s.c)
	s.c = 0
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
	d.wakeGuarded()
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

// Credentials are the peer credentials of a client, read from the socket
// (SO_PEERCRED) when it connected. UID/GID are effective IDs in the server's
// namespaces; an unmappable ID is the namespace overflow ID. PID may be zero
// even for a valid client when its PID namespace is not visible to the server.
type Credentials struct {
	PID int
	UID uint32
	GID uint32
}

var credScratch [3]int32

var errInvalidClient = errors.New("purego-libwayland: invalid client")

// Credentials returns the peer credentials of the client. It returns an error
// for an invalid (zero) Client. It does not allocate. Display goroutine only.
func (c Client) Credentials() (Credentials, error) {
	if c.c == 0 {
		return Credentials{}, errInvalidClient
	}
	// Package-level scratch (display goroutine only) avoids heap-escaping locals.
	purego.Syscall6(symClientGetCredentials, c.c, uintptr(unsafe.Pointer(&credScratch[0])), uintptr(unsafe.Pointer(&credScratch[1])), uintptr(unsafe.Pointer(&credScratch[2])), 0, 0)
	return Credentials{PID: int(credScratch[0]), UID: uint32(credScratch[1]), GID: uint32(credScratch[2])}, nil
}

// PID is the process ID of the client, from Credentials. It is zero for an
// invalid client or a peer outside the server's visible PID namespaces.
// Display goroutine only.
func (c Client) PID() int {
	cr, err := c.Credentials()
	if err != nil {
		return 0
	}
	return cr.PID
}

func (c Client) CreateResource(iface *Interface, version int32, id uint32, h Handler) (*Resource, error) {
	rc := wlResourceCreate(c.c, iface.c, version, id)
	if rc == 0 {
		return nil, fmt.Errorf("purego-libwayland: wl_resource_create(%s) failed", iface.Name)
	}
	// id 0 asks libwayland to allocate a server-side ID.
	if id == 0 {
		id = wlResourceGetID(rc)
	}
	r := &Resource{c: rc, id: id, version: version, client: c.c, iface: iface, handler: h}
	live.resources[rc] = r
	wlResourceSetDispatcher(rc, cbDispatcher, implMarker(), 0, cbDestroy)
	return r, nil
}

func (r *Resource) Alive() bool { return !r.gone }
func (r *Resource) ID() uint32 {
	if r.gone {
		return 0
	}
	return r.id
}
func (r *Resource) Version() int32 {
	if r.gone {
		return 0
	}
	return r.version
}
func (r *Resource) Client() Client {
	if r.gone {
		return Client{}
	}
	return Client{r.client}
}
func (r *Resource) Iface() *Interface { return r.iface }

// Destroy frees the resource. It is a no-op once the resource is gone, so
// generated destructor requests and handlers may both call it.
func (r *Resource) Destroy() {
	if !r.gone {
		wlResourceDestroy(r.c)
		// A bare handle gets no destroy callback to mark it gone.
		r.gone = r.gone || r.bare
	}
}

// eventArgs holds the wl_argument array of the event being posted.
// libwayland copies it before wl_resource_post_event_array returns.
// Display goroutine only; post_event_array does not call back into Go, so
// PostEvent is not re-entrant. No protocol event has more than 8 arguments.
var eventArgs [16]Arg

// PostEvent sends an event. Arguments must match the event signature; build
// string and array arguments with a Pinner and unpin it after this returns.
// Calling it after destruction is safe and does nothing.
func (r *Resource) PostEvent(opcode uint32, args ...Arg) {
	if r.gone {
		return
	}
	if len(args) > len(eventArgs) {
		panic("purego-libwayland: too many event arguments")
	}
	var p unsafe.Pointer
	if len(args) > 0 {
		copy(eventArgs[:], args)
		p = unsafe.Pointer(&eventArgs[0])
	}
	wlResourcePostEventArr(r.c, opcode, p)
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

func dispatch(_ uintptr, target uintptr, opcode uint32, _ unsafe.Pointer, args unsafe.Pointer) int32 {
	r := live.resources[target]
	if r == nil || opcode >= uint32(len(r.iface.Requests)) {
		return 0
	}
	m := &r.iface.Requests[opcode]
	if r.handler == nil {
		closeRequestFDs(m.fdPositions, args)
		return 0
	}
	var a []Arg
	if m.argc > 0 {
		a = unsafe.Slice((*Arg)(args), m.argc)
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

// fdReadable dispatches an AddFD source (wl_event_loop_fd_func_t).
func fdReadable(_ int32, mask uint32, data uintptr) int32 {
	if s := live.fds[data]; s != nil {
		if mask&(1|4|8) != 0 {
			s.onReadable()
		}
		if s.c != 0 && mask&2 != 0 && s.onWritable != nil {
			s.onWritable()
		}
	}
	return 0
}

func bind(client uintptr, data uintptr, version uint32, id uint32) {
	if fn := live.binds[data]; fn != nil {
		fn(Client{client}, version, id)
	}
}

// closeRequestFDs takes ownership of fd arguments when there is no handler.
func closeRequestFDs(positions []int, args unsafe.Pointer) {
	for _, i := range positions {
		_ = unix.Close((*Arg)(unsafe.Add(args, i*8)).Fd())
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
