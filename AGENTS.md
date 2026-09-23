# Repository rules

- Go 1.27 only.
- Library code must build with `CGO_ENABLED=0`.
- Use only non-varargs libwayland entry points (no `va_list`).
- Make all libwayland calls on one OS-locked goroutine.
- C-visible memory must never be Go-GC-managed.
- Use signed conventional commits: `type(scope): description`.
