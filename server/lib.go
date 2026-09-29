package server

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/bnema/purego"
	"golang.org/x/sys/unix"
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

	symDisplayFlushClients, symEventLoopDispatch, symResourceCreate                         uintptr
	symResourceSetDispatcher, symResourcePostEventArr, symResourceDestroy, symResourceGetID uintptr
	symErrnoLocation                                                                        uintptr
	// wl_fixes support (libwayland 1.26+); zero on older libraries.
	symFixesAckGlobalRemove uintptr

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
			{"wl_resource_get_id", &symResourceGetID},
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
		symFixesAckGlobalRemove, _ = purego.Dlsym(lib, "wl_fixes_handle_ack_global_remove")
		symErrnoLocation, err = purego.Dlsym(lib, "__errno_location")
		if err != nil {
			libc, openErr := purego.Dlopen("libc.so.6", purego.RTLD_NOW|purego.RTLD_GLOBAL)
			if openErr != nil {
				loadErr = fmt.Errorf("purego-libwayland: open libc.so.6: %w", openErr)
				return
			}
			symErrnoLocation, err = purego.Dlsym(libc, "__errno_location")
			if err != nil {
				loadErr = fmt.Errorf("purego-libwayland: resolve __errno_location: %w", err)
				return
			}
		}

		cbDispatcher = purego.NewCallbackInts(func(a *purego.CallbackArgs) uintptr {
			// C memory owned by libwayland; reinterpret pointer bits without uintptr-to-pointer conversion.
			msg, args := a.Int(3), a.Int(4)
			return uintptr(uint32(dispatch(a.Int(0), a.Int(1), uint32(a.Int(2)), *(*unsafe.Pointer)(unsafe.Pointer(&msg)), *(*unsafe.Pointer)(unsafe.Pointer(&args)))))
		})
		cbDestroy = purego.NewCallbackInts(func(a *purego.CallbackArgs) uintptr {
			destroyed(a.Int(0))
			return 0
		})
		cbBind = purego.NewCallbackInts(func(a *purego.CallbackArgs) uintptr {
			bind(a.Int(0), a.Int(1), uint32(a.Int(2)), uint32(a.Int(3)))
			return 0
		})
		cbWake = purego.NewCallbackInts(func(a *purego.CallbackArgs) uintptr {
			return uintptr(uint32(wake(int32(a.Int(0)), uint32(a.Int(1)), a.Int(2))))
		})
	})
	return loadErr
}

// Integer and pointer-only entry points avoid RegisterLibFunc's reflective call path.
func wlDisplayFlushClients(display uintptr) {
	purego.Syscall6(symDisplayFlushClients, display, 0, 0, 0, 0, 0)
}
func wlEventLoopDispatch(loop uintptr, timeout int32) int32 {
	r, _, _ := purego.Syscall6(symEventLoopDispatch, loop, uintptr(timeout), 0, 0, 0, 0)
	return int32(r)
}
func lastErrno() unix.Errno {
	p, _, _ := purego.Syscall6(symErrnoLocation, 0, 0, 0, 0, 0, 0)
	// C memory owned by libc; reinterpret pointer bits without uintptr-to-pointer conversion.
	return unix.Errno(**(**int32)(unsafe.Pointer(&p)))
}
func wlResourceCreate(client, iface uintptr, version int32, id uint32) uintptr {
	r, _, _ := purego.Syscall6(symResourceCreate, client, iface, uintptr(version), uintptr(id), 0, 0)
	return r
}
func wlResourceGetID(resource uintptr) uint32 {
	r, _, _ := purego.Syscall6(symResourceGetID, resource, 0, 0, 0, 0, 0)
	return uint32(r)
}
func wlResourceSetDispatcher(resource, dispatcher, impl, data, destroy uintptr) {
	purego.Syscall6(symResourceSetDispatcher, resource, dispatcher, impl, data, destroy, 0)
}
func wlResourcePostEventArr(resource uintptr, opcode uint32, args unsafe.Pointer) {
	purego.Syscall6(symResourcePostEventArr, resource, uintptr(opcode), uintptr(args), 0, 0, 0)
}
func wlResourceDestroy(resource uintptr) {
	purego.Syscall6(symResourceDestroy, resource, 0, 0, 0, 0, 0)
}
func wlFixesHandleAckGlobalRemove(fixes, registry uintptr, name uint32) {
	purego.Syscall6(symFixesAckGlobalRemove, fixes, registry, uintptr(name), 0, 0, 0)
}
