package server

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/bnema/purego"
	"golang.org/x/sys/unix"
)

// FD returns the client's connection socket. The descriptor is borrowed from
// libwayland: the caller must not close it, and it is valid only until the
// client is destroyed (see OnDestroy). Use it to identify or inspect the
// connection (for example fstat or /proc/self/fdinfo); reading or writing it
// would corrupt the protocol stream. It returns an error for an invalid
// (zero) Client. It does not allocate. Display goroutine only.
func (c Client) FD() (int, error) {
	if c.c == 0 {
		return -1, errInvalidClient
	}
	r, _, _ := purego.Syscall6(symClientGetFD, c.c, 0, 0, 0, 0, 0)
	return int(int32(r)), nil
}

// CreateClient adopts fd, an already connected stream socket (for example one
// accepted on a listening socket the compositor owns), as a new client served
// by this display.
//
// Ownership of fd: on success libwayland owns it and closes it when the
// client is destroyed; the caller must not close it, nor use it for I/O. On
// failure libwayland does not take it (it is left open) and the caller keeps
// ownership and must close it. Sockets should be close-on-exec (accept with
// SOCK_CLOEXEC); libwayland does not set the flag. fd must be a connected
// unix socket: libwayland reads its peer credentials with SO_PEERCRED. A
// socket whose peer has already closed is still adopted; the client is then
// destroyed on the next dispatch.
//
// On failure the error wraps the errno libwayland left (for example ENOTSOCK
// or ENOMEM), read right after the call on the same thread; it is reliable
// when called on the locked display goroutine.
//
// The returned Client is valid until destroyed; register OnDestroy to learn
// when. Display goroutine only, or before Run starts.
func (d *Display) CreateClient(fd int) (Client, error) {
	if d == nil || d.c == 0 {
		return Client{}, errors.New("purego-libwayland: invalid display")
	}
	if fd < 0 {
		return Client{}, fmt.Errorf("purego-libwayland: wl_client_create: invalid fd %d", fd)
	}
	r, _, _ := purego.Syscall6(symClientCreate, d.c, uintptr(fd), 0, 0, 0, 0)
	if r == 0 {
		return Client{}, fmt.Errorf("purego-libwayland: wl_client_create(%d) failed, fd not adopted: %w", fd, lastErrno())
	}
	return Client{r}, nil
}

// OnDestroy arranges for fn to run once when the client is destroyed, whether
// it disconnected, was disconnected after a protocol error, or the display was
// closed or stopped. Any number of functions may be registered per client;
// each runs once, in registration order. There is no way to unregister one;
// make fn a cheap no-op instead if it became irrelevant.
//
// fn runs on the display goroutine inside libwayland's client destruction. The
// Client is still valid during fn (FD and Credentials work, resources are not
// yet destroyed) but is dead afterwards: drop every copy of it, for example
// delete it from a map[Client]T, because the handle value may be reused by a
// later client.
//
// Registering another listener on the same client from inside fn is not
// supported: whether it would run depends on how libwayland's final emit walks
// the listener list (it differs between versions). Registering on a different,
// live client from inside fn is fine.
//
// A Client is a comparable value (a handle to the C object) and is safe as a
// map key from its creation or bind until fn runs.
//
// OnDestroy does nothing for a zero Client or nil fn. Registering on an
// already destroyed client is invalid. It panics if the memory for the
// listener cannot be mapped. Display goroutine only, or before Run starts.
func (c Client) OnDestroy(fn func()) {
	if c.c == 0 || fn == nil {
		return
	}
	l := listeners.get()
	live.clientGone[l] = fn
	purego.Syscall6(symClientAddDestroyListener, c.c, l, 0, 0, 0, 0)
}

// listenerSlots hands out wl_listener structs in C-visible memory that the Go
// GC cannot move or free. A slot is a struct wl_listener ({wl_list link;
// wl_notify_func_t notify}, 24 bytes on LP64), padded to 32. Slots are
// recycled through a free list, so only the peak number of concurrently
// registered listeners is ever mapped; the chunks themselves are never
// unmapped. Display goroutine only.
type listenerSlots struct {
	chunks [][]byte
	free   []uintptr
}

const (
	listenerSlot      = 32
	listenerChunkSize = 4096
)

var listeners listenerSlots

func (s *listenerSlots) get() uintptr {
	if len(s.free) == 0 {
		mem, err := unix.Mmap(-1, 0, listenerChunkSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
		if err != nil {
			panic(fmt.Errorf("purego-libwayland: listener mmap: %w", err))
		}
		s.chunks = append(s.chunks, mem)
		base := uintptr(unsafe.Pointer(&mem[0]))
		for off := listenerChunkSize - listenerSlot; off >= 0; off -= listenerSlot {
			s.free = append(s.free, base+uintptr(off))
		}
	}
	l := s.free[len(s.free)-1]
	s.free = s.free[:len(s.free)-1]
	// notify lives at offset 16; wl_client_add_destroy_listener fills link.
	// l is an address inside an mmap chunk kept in s.chunks.
	*(*uintptr)(unsafe.Add(*(*unsafe.Pointer)(unsafe.Pointer(&l)), 16)) = cbClientGone
	return l
}

func (s *listenerSlots) put(l uintptr) { s.free = append(s.free, l) }

// clientGone is the wl_notify_func_t of every client destroy listener.
func clientGone(l uintptr) {
	fn := live.clientGone[l]
	delete(live.clientGone, l)
	// libwayland's emit loop first unlinks each node and, on current versions,
	// resets it with wl_list_init, so this is a no-op on a self-linked node;
	// on older versions the node may still be linked and must be unlinked
	// before the slot can be reused.
	purego.Syscall6(symListRemove, l, 0, 0, 0, 0, 0)
	listeners.put(l)
	if fn != nil {
		fn()
	}
}
