# purego-libwayland

Go bindings for libwayland without cgo, loaded at runtime through github.com/bnema/purego.

## Status

Early v0.x. Server side first; client side planned.

## Requirements

Linux, Go 1.27, and `libwayland-server.so.0` at runtime.

## Build

```sh
CGO_ENABLED=0 go build ./...
```

## License

MIT.
