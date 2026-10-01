# purego-libwayland

Go bindings for libwayland without cgo, loaded at runtime through github.com/bnema/purego.

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

## License

MIT, see `LICENSE`.
