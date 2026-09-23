package server

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// arena is C-visible memory the Go GC never moves or frees. It holds
// wl_interface and wl_message tables and their strings for the whole
// process lifetime.
type arena struct {
	mem  []byte
	base uintptr
	off  int
}

func newArena(size int) (*arena, error) {
	mem, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return nil, fmt.Errorf("arena: mmap: %w", err)
	}
	return &arena{mem: mem, base: uintptr(unsafe.Pointer(&mem[0]))}, nil
}

// alloc returns the offset and C address of n zeroed, 8-byte aligned bytes.
func (a *arena) alloc(n int) (int, uintptr) {
	a.off = (a.off + 7) &^ 7
	if a.off+n > len(a.mem) {
		panic("purego-libwayland: arena exhausted")
	}
	off := a.off
	a.off += n
	return off, a.base + uintptr(off)
}

func (a *arena) cstring(s string) uintptr {
	off, addr := a.alloc(len(s) + 1)
	copy(a.mem[off:], s)
	return addr
}

func (a *arena) putPtr(off int, v uintptr) { binary.LittleEndian.PutUint64(a.mem[off:], uint64(v)) }
func (a *arena) putI32(off int, v int32)   { binary.LittleEndian.PutUint32(a.mem[off:], uint32(v)) }
