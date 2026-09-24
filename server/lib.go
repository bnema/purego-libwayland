package server

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/bnema/purego"
)

// libwayland-server entry points. Only non-varargs functions are bound.
var (
	wlDisplayCreate         func() uintptr
	wlDisplayDestroy        func(display uintptr)
	wlDisplayDestroyClients func(display uintptr)
	wlDisplayAddSocketFD    func(display uintptr, fd int32) int32
	wlDisplayGetEventLoop   func(display uintptr) uintptr
	wlDisplayFlushClients   func(display uintptr)
	wlEventLoopDispatch     func(loop uintptr, timeout int32) int32
	wlEventLoopGetFD        func(loop uintptr) int32
	wlEventLoopAddFD        func(loop uintptr, fd int32, mask uint32, fn, data uintptr) uintptr
	wlEventSourceRemove     func(source uintptr) int32
	wlGlobalCreate          func(display, iface uintptr, version int32, data uintptr, bind uintptr) uintptr
	wlResourceCreate        func(client, iface uintptr, version int32, id uint32) uintptr
	wlResourceSetDispatcher func(resource, dispatcher, impl, data, destroy uintptr)
	wlResourcePostEventArr  func(resource uintptr, opcode uint32, args unsafe.Pointer)
	wlResourcePostError     func(resource uintptr, code uint32, msg uintptr)
	wlResourceDestroy       func(resource uintptr)
	wlResourceGetID         func(resource uintptr) uint32
	wlResourceGetClient     func(resource uintptr) uintptr
	wlResourceGetVersion    func(resource uintptr) int32
)

var (
	loadOnce sync.Once
	loadErr  error

	// Shared C callbacks. purego callbacks are never freed, so there is one
	// of each for the whole process.
	cbDispatcher uintptr
	cbDestroy    uintptr
	cbBind       uintptr
	cbWake       uintptr
)

func load() error {
	loadOnce.Do(func() {
		lib, err := purego.Dlopen("libwayland-server.so.0", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = fmt.Errorf("purego-libwayland: open libwayland-server.so.0: %w", err)
			return
		}
		defer func() {
			if r := recover(); r != nil {
				loadErr = fmt.Errorf("purego-libwayland: %v", r)
			}
		}()
		reg := func(fptr any, name string) { purego.RegisterLibFunc(fptr, lib, name) }
		reg(&wlDisplayCreate, "wl_display_create")
		reg(&wlDisplayDestroy, "wl_display_destroy")
		reg(&wlDisplayDestroyClients, "wl_display_destroy_clients")
		reg(&wlDisplayAddSocketFD, "wl_display_add_socket_fd")
		reg(&wlDisplayGetEventLoop, "wl_display_get_event_loop")
		reg(&wlDisplayFlushClients, "wl_display_flush_clients")
		reg(&wlEventLoopDispatch, "wl_event_loop_dispatch")
		reg(&wlEventLoopGetFD, "wl_event_loop_get_fd")
		reg(&wlEventLoopAddFD, "wl_event_loop_add_fd")
		reg(&wlEventSourceRemove, "wl_event_source_remove")
		reg(&wlGlobalCreate, "wl_global_create")
		reg(&wlResourceCreate, "wl_resource_create")
		reg(&wlResourceSetDispatcher, "wl_resource_set_dispatcher")
		reg(&wlResourcePostEventArr, "wl_resource_post_event_array")
		reg(&wlResourcePostError, "wl_resource_post_error")
		reg(&wlResourceDestroy, "wl_resource_destroy")
		reg(&wlResourceGetID, "wl_resource_get_id")
		reg(&wlResourceGetClient, "wl_resource_get_client")
		reg(&wlResourceGetVersion, "wl_resource_get_version")

		cbDispatcher = purego.NewCallback(dispatch)
		cbDestroy = purego.NewCallback(destroyed)
		cbBind = purego.NewCallback(bind)
		cbWake = purego.NewCallback(wake)
	})
	return loadErr
}
