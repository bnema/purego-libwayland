package server

import (
	"math"
	"runtime"
	"unsafe"
)

// Arg is one raw union wl_argument value.
type Arg uint64

// Fixed is a wl_fixed_t: a signed 24.8 fixed-point number.
type Fixed int32

func FixedFromFloat(v float64) Fixed { return Fixed(math.Round(v * 256)) }
func (f Fixed) Float() float64       { return float64(f) / 256 }

// Argument constructors for PostEvent.
func Uint(v uint32) Arg    { return Arg(v) }
func Int(v int32) Arg      { return Arg(uint32(v)) }
func FixedArg(v Fixed) Arg { return Arg(uint32(v)) }

// Fd passes a file descriptor. libwayland duplicates it, so the caller keeps
// ownership.
func Fd(fd int) Arg { return Arg(uint32(int32(fd))) }

// Object passes a resource, or a null object when r is nil.
func Object(r *Resource) Arg {
	if r == nil {
		return 0
	}
	return Arg(r.c)
}

// NewID passes a resource created for a new_id event argument.
func NewID(r *Resource) Arg { return Object(r) }

// String passes a string. The bytes stay pinned until p is unpinned, which
// must happen after PostEvent returns. With null set, "" is sent as NULL.
func String(p *runtime.Pinner, s string, null bool) Arg {
	if null && s == "" {
		return 0
	}
	b := make([]byte, len(s)+1)
	copy(b, s)
	p.Pin(&b[0])
	return Arg(uintptr(unsafe.Pointer(&b[0])))
}

// wlArray matches struct wl_array on LP64 (size_t, size_t, void *).
type wlArray struct {
	size  uintptr
	alloc uintptr
	data  unsafe.Pointer
}

// Array passes a byte array. See String for the pinning rule.
func Array(p *runtime.Pinner, b []byte) Arg {
	a := &wlArray{size: uintptr(len(b)), alloc: uintptr(len(b))}
	if len(b) > 0 {
		p.Pin(&b[0])
		a.data = unsafe.Pointer(&b[0])
	}
	p.Pin(a)
	return Arg(uintptr(unsafe.Pointer(a)))
}

// Request argument accessors. Values are only valid during dispatch; the
// String and Array accessors copy.
func (a Arg) Uint() uint32        { return uint32(a) }
func (a Arg) Int() int32          { return int32(uint32(a)) }
func (a Arg) Fixed() Fixed        { return Fixed(int32(uint32(a))) }
func (a Arg) NewID() uint32       { return uint32(a) }

// Resource returns the object argument, nil for a null object. An object
// libwayland created itself (wl_registry, wl_callback of wl_display) comes
// back as a bare handle: Destroy works, it has no handler, version or client.
// Like other arguments, a bare handle is only valid during the dispatch; do
// not retain it.
func (a Arg) Resource() *Resource {
	if r := live.resources[uintptr(a)]; r != nil || a == 0 {
		return r
	}
	return &Resource{c: uintptr(a), bare: true}
}

// Fd returns a received file descriptor. The handler owns it and must close it.
func (a Arg) Fd() int { return int(int32(uint32(a))) }

// String copies a received string; NULL becomes "".
func (a Arg) String() string {
	p := *(*unsafe.Pointer)(unsafe.Pointer(&a)) // C memory owned by libwayland
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(p), n))
}

// Array copies a received wl_array.
func (a Arg) Array() []byte {
	arr := (*wlArray)(*(*unsafe.Pointer)(unsafe.Pointer(&a))) // C memory owned by libwayland
	if arr == nil || arr.size == 0 {
		return nil
	}
	return append([]byte(nil), unsafe.Slice((*byte)(arr.data), arr.size)...)
}
