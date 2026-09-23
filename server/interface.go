package server

// Message describes one request or event. Signature uses the libwayland
// format ("n", "?oii", ...). Types holds one entry per argument; entries are
// non-nil only for typed new_id and object arguments.
type Message struct {
	Name      string
	Signature string
	Types     []*Interface
}

// Interface describes a protocol interface. Build it once with
// NewInterfaces; its C table lives for the process lifetime.
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

var tables *arena

// NewInterfaces writes C tables for a set of interfaces that may reference
// each other. It must be called before any interface is used.
func NewInterfaces(ifaces ...*Interface) error {
	if err := load(); err != nil {
		return err
	}
	if tables == nil {
		a, err := newArena(1 << 20)
		if err != nil {
			return err
		}
		tables = a
	}
	a := tables
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

func writeMessages(a *arena, msgs []Message) uintptr {
	if len(msgs) == 0 {
		return 0
	}
	off, addr := a.alloc(sizeMessage * len(msgs))
	for i, m := range msgs {
		var types uintptr
		if len(m.Types) > 0 {
			toff, taddr := a.alloc(8 * len(m.Types))
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
