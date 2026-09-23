package main

import (
	"strings"
	"testing"
)

func TestUntypedNewIDEvent(t *testing.T) {
	g := &gen{owners: map[string]string{"test_device": ""}, imports: map[string]string{}}
	b, err := g.generate(protocol{Interfaces: []iface{{Name: "test_device", Version: 1, Events: []message{{Name: "created", Args: []arg{{Name: "id", Type: "new_id"}}}}}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `Signature: "sun"`) || !strings.Contains(s, `server.String(&p, id.Iface().Name, false), server.Uint(uint32(id.Version())), server.NewID(id)`) {
		t.Fatalf("untyped new_id event missing wire args:\n%s", s)
	}
}

func TestSignature(t *testing.T) {
	cases := []struct {
		m    message
		want string
	}{
		{message{Name: "bind", Args: []arg{{Type: "uint"}, {Type: "new_id"}}}, "usun"},
		{message{Since: 2, Args: []arg{{Type: "object", Nullable: true}, {Type: "fd"}, {Type: "fixed"}}}, "2?ohf"},
		{message{Args: []arg{{Type: "new_id", Interface: "wl_surface"}}}, "n"},
	}
	for _, c := range cases {
		if got := signature(c.m); got != c.want {
			t.Errorf("signature(%+v)=%q, want %q", c.m, got, c.want)
		}
	}
}
