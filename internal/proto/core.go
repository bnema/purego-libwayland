// Package proto holds hand-written core interfaces for the feasibility
// prototype. The generator replaces this package.
package proto

import (
	"sync"

	"github.com/bnema/purego-libwayland/server"
)

var (
	Compositor = &server.Interface{Name: "wl_compositor", Version: 1}
	Surface    = &server.Interface{Name: "wl_surface", Version: 1}
	Region     = &server.Interface{Name: "wl_region", Version: 1}
	Callback   = &server.Interface{Name: "wl_callback", Version: 1}
)

// Opcodes used by the prototype.
const (
	CompositorCreateSurface = 0
	CompositorCreateRegion  = 1
	SurfaceDestroy          = 0
	SurfaceFrame            = 3
	SurfaceCommit           = 6
	RegionDestroy           = 0
	CallbackDone            = 0
)

var initOnce = sync.OnceValue(initInterfaces)

// Init builds the C tables once per process.
func Init() error { return initOnce() }

func initInterfaces() error {
	Compositor.Requests = []server.Message{
		{Name: "create_surface", Signature: "n", Types: []*server.Interface{Surface}},
		{Name: "create_region", Signature: "n", Types: []*server.Interface{Region}},
	}
	Surface.Requests = []server.Message{
		{Name: "destroy", Signature: ""},
		{Name: "attach", Signature: "?oii", Types: []*server.Interface{nil, nil, nil}},
		{Name: "damage", Signature: "iiii"},
		{Name: "frame", Signature: "n", Types: []*server.Interface{Callback}},
		{Name: "set_opaque_region", Signature: "?o", Types: []*server.Interface{Region}},
		{Name: "set_input_region", Signature: "?o", Types: []*server.Interface{Region}},
		{Name: "commit", Signature: ""},
	}
	Region.Requests = []server.Message{
		{Name: "destroy", Signature: ""},
		{Name: "add", Signature: "iiii"},
		{Name: "subtract", Signature: "iiii"},
	}
	Callback.Events = []server.Message{{Name: "done", Signature: "u"}}
	return server.NewInterfaces(Compositor, Surface, Region, Callback)
}
