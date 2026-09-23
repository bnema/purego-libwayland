package server

import (
	"testing"
	"unsafe"
)

func TestForwardReferenceCTable(t *testing.T) {
	first := &Interface{Name: "test_first", Version: 1}
	later := &Interface{Name: "test_later", Version: 1}
	first.Requests = []Message{{Name: "make", Signature: "n", Types: []*Interface{later}}}
	if err := NewInterfaces(first, later); err != nil {
		t.Fatal(err)
	}
	messages := *(*unsafe.Pointer)(unsafe.Add(unsafe.Pointer(&tables.mem[0]), first.c-tables.base+16))
	types := *(*unsafe.Pointer)(unsafe.Add(messages, 16))
	if types == nil {
		t.Fatal("missing C types array")
	}
	target := uintptr(*(*unsafe.Pointer)(types))
	if target == 0 || target != later.c {
		t.Fatalf("forward reference C types: target=%#x want=%#x", target, later.c)
	}
}
