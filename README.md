# purego-libwayland

Go bindings for libwayland-server without cgo, loaded at runtime through github.com/bnema/purego.

## Status

Early v0.x. A server-side libwayland runtime (`server/` package).

## Requirements

Linux, Go 1.27, and `libwayland-server.so.0` at runtime.

## Build

```sh
CGO_ENABLED=0 go build ./...
```

## Protocol bindings

Generated protocol bindings live in [github.com/bnema/go-wayland-bindings](https://github.com/bnema/go-wayland-bindings), one package per protocol under `server/<pkg>`, for example `server/xdgshell` and `server/wlrlayershell`. They build on this runtime's `server` package.

## Display integration

`Display.SetGlobalFilter(func(Client, *Global) bool)` controls which clients see
and bind each global; passing `nil` removes the filter. Set it before clients
receive restricted globals. Global handles are stable, including during creation;
filters must not create globals or block.

`Display.AddFDRW(fd, onReadable, onWritable)` registers a non-blocking fd with
writable interest initially disabled. `Source.SetWritable(true)` arms writable
callbacks after `EAGAIN`; `SetWritable(false)` disarms them when output drains.
Both return errors on registration/update failures. Readable interest remains
active. Callbacks run on the display goroutine with no dispatch allocations.
`AddFD` remains the readable-only API. Hangup/error readiness calls onReadable;
remove the source on EOF/error. The caller owns the original fd.

These APIs, like other libwayland operations, belong on the display's OS-locked
goroutine (use `Do` or `Post` from other goroutines).

## License

MIT, see `LICENSE`.
