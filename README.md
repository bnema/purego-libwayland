# purego-libwayland

Go bindings for libwayland without cgo, loaded at runtime through github.com/bnema/purego.

## Status

Early v0.x. Server bindings and generated Wayland protocol packages are available.

## Requirements

Linux, Go 1.27, and `libwayland-server.so.0` at runtime.

## Build

```sh
CGO_ENABLED=0 go build ./...
```

## Protocol generation

Vendored protocol XML lives in `protocols/`. Run `make generate` to regenerate the Go packages in `protocol/`.

## License

MIT.
