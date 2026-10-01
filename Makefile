.PHONY: test vet race check

test:
	CGO_ENABLED=0 go test ./...

vet:
	CGO_ENABLED=0 go vet ./...

# The race detector needs cgo only for the test binary; library code stays cgo-free.
race:
	CGO_ENABLED=1 go test -race ./...

check: vet test
