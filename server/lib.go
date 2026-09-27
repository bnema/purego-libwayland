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
	wlEventLoopGetFD        func(loop uintptr) int32
	wlEventLoopAddFD        func(loop uintptr, fd int32, mask uint32, fn, data uintptr) uintptr
	wlEventSourceRemove     func(source uintptr) int32
	wlGlobalCreate          func(display, iface uintptr, version int32, data uintptr, bind uintptr) uintptr
	wlGlobalRemove          func(global uintptr)
	wlResourcePostError     func(resource uintptr, code uint32, msg uintptr)
	wlClientGetCredentials  func(client uintptr, pid, uid, gid unsafe.Pointer)
)

var (
	loadOnce sync.Once
	loadErr  error

	symDisplayFlushClients, symEventLoopDispatch, symResourceCreate       uintptr
	symResourceSetDispatcher, symResourcePostEventArr, symResourceDestroy uintptr

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
		reg(&wlEventLoopGetFD, "wl_event_loop_get_fd")
		reg(&wlEventLoopAddFD, "wl_event_loop_add_fd")
		reg(&wlEventSourceRemove, "wl_event_source_remove")
		reg(&wlGlobalCreate, "wl_global_create")
		reg(&wlGlobalRemove, "wl_global_remove")
		reg(&wlResourcePostError, "wl_resource_post_error")
		reg(&wlClientGetCredentials, "wl_client_get_credentials")
		for _, entry := range []struct {
			name string
			sym  *uintptr
		}{
			{"wl_display_flush_clients", &symDisplayFlushClients},
			{"wl_event_loop_dispatch", &symEventLoopDispatch},
			{"wl_resource_create", &symResourceCreate},
			{"wl_resource_set_dispatcher", &symResourceSetDispatcher},
			{"wl_resource_post_event_array", &symResourcePostEventArr},
			{"wl_resource_destroy", &symResourceDestroy},
		} {
			*entry.sym, err = purego.Dlsym(lib, entry.name)
			if err != nil {
				loadErr = fmt.Errorf("purego-libwayland: resolve %s: %w", entry.name, err)
				return
			}
		}

		cbDispatcher = purego.NewCallback(dispatch)
		cbDestroy = purego.NewCallback(destroyed)
		cbBind = purego.NewCallback(bind)
		cbWake = purego.NewCallback(wake)
	})
	return loadErr
}

// callArgs is scratch for SyscallN so hot calls do not allocate a variadic
// slice. Display goroutine only; all libwayland calls run there.
var callArgs [6]uintptr

// Integer and pointer-only entry points avoid RegisterLibFunc's reflective call path.
func wlDisplayFlushClients(display uintptr) {
	callArgs[0] = display
	purego.SyscallN(symDisplayFlushClients, callArgs[:1]...)
}
func wlEventLoopDispatch(loop uintptr, timeout int32) int32 {
	callArgs[0], callArgs[1] = loop, uintptr(timeout)
	r, _, _ := purego.SyscallN(symEventLoopDispatch, callArgs[:2]...)
	return int32(r)
}
func wlResourceCreate(client, iface uintptr, version int32, id uint32) uintptr {
	callArgs[0], callArgs[1], callArgs[2], callArgs[3] = client, iface, uintptr(version), uintptr(id)
	r, _, _ := purego.SyscallN(symResourceCreate, callArgs[:4]...)
	return r
}
func wlResourceSetDispatcher(resource, dispatcher, impl, data, destroy uintptr) {
	callArgs[0], callArgs[1], callArgs[2], callArgs[3], callArgs[4] = resource, dispatcher, impl, data, destroy
	purego.SyscallN(symResourceSetDispatcher, callArgs[:5]...)
}
func wlResourcePostEventArr(resource uintptr, opcode uint32, args unsafe.Pointer) {
	callArgs[0], callArgs[1], callArgs[2] = resource, uintptr(opcode), uintptr(args)
	purego.SyscallN(symResourcePostEventArr, callArgs[:3]...)
}
func wlResourceDestroy(resource uintptr) {
	callArgs[0] = resource
	purego.SyscallN(symResourceDestroy, callArgs[:1]...)
}
