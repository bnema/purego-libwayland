package server

import "sync"

// Message describes one request or event. Signature uses the libwayland
// format ("n", "?oii", "2u", ...). Types holds one entry per argument, and may
// be shorter than the argument list; entries are non-nil only for typed
// new_id and object arguments.
type Message struct {
	Name      string
	Signature string
	Types     []*Interface
}

// Interface describes a protocol interface. Build its C table once with
// NewInterfaces; the table lives for the process lifetime.
type Interface struct {
	Name     string
	Version  int32
	Requests []Message
	Events   []Message

	c uintptr // struct wl_interface*
}

// C layout (LP64):
//
//	struct wl_message   { const char *name; const char *signature; const struct wl_interface **types; } // 24 bytes
//	struct wl_interface { const char *name; int version; int method_count; const struct wl_message *methods;
//	                      int event_count; const struct wl_message *events; }                             // 40 bytes
const (
	sizeMessage   = 24
	sizeInterface = 40
)

var (
	tablesMu sync.Mutex
	tables   arena
)

// NewInterfaces writes C tables for a set of interfaces that may reference
// each other. Interfaces from other sets must already be built. It does not
// load libwayland, so generated packages call it from init.
func NewInterfaces(ifaces ...*Interface) error {
	tablesMu.Lock()
	defer tablesMu.Unlock()
	if err := tables.reserve(tableSize(ifaces)); err != nil {
		return err
	}
	a := &tables
	// Pass 1: reserve every wl_interface so cross references resolve.
	offs := make([]int, len(ifaces))
	for i, it := range ifaces {
		offs[i], it.c = a.alloc(sizeInterface)
	}
	// Pass 2: fill messages and headers.
	for i, it := range ifaces {
		o := offs[i]
		a.putPtr(o, a.cstring(it.Name))
		a.putI32(o+8, it.Version)
		a.putI32(o+12, int32(len(it.Requests)))
		a.putPtr(o+16, writeMessages(a, it.Requests))
		a.putI32(o+24, int32(len(it.Events)))
		a.putPtr(o+32, writeMessages(a, it.Events))
	}
	return nil
}

// tableSize is an upper bound of the arena bytes NewInterfaces uses,
// counting 7 bytes of alignment padding per allocation.
func tableSize(ifaces []*Interface) int {
	n := 0
	for _, it := range ifaces {
		n += sizeInterface + 7 + len(it.Name) + 1 + 7
		for _, msgs := range [][]Message{it.Requests, it.Events} {
			n += sizeMessage*len(msgs) + 7
			for _, m := range msgs {
				n += len(m.Name) + 1 + 7 + len(m.Signature) + 1 + 7 + 8*argCount(m.Signature) + 7
			}
		}
	}
	return n
}

// writeMessages writes a wl_message array. Every message with arguments gets
// a types array, because libwayland reads it for each object and new_id
// argument even when the interface is unknown.
func writeMessages(a *arena, msgs []Message) uintptr {
	if len(msgs) == 0 {
		return 0
	}
	off, addr := a.alloc(sizeMessage * len(msgs))
	for i, m := range msgs {
		var types uintptr
		if n := argCount(m.Signature); n > 0 {
			if len(m.Types) > n {
				panic("purego-libwayland: " + m.Name + " has more types than arguments")
			}
			toff, taddr := a.alloc(8 * n)
			for j, t := range m.Types {
				if t != nil {
					if t.c == 0 {
						panic("purego-libwayland: interface " + t.Name + " referenced before NewInterfaces")
					}
					a.putPtr(toff+8*j, t.c)
				}
			}
			types = taddr
		}
		mo := off + i*sizeMessage
		a.putPtr(mo, a.cstring(m.Name))
		a.putPtr(mo+8, a.cstring(m.Signature))
		a.putPtr(mo+16, types)
	}
	return addr
}
