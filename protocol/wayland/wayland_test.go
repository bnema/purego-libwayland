package wayland

import (
	"github.com/bnema/purego-libwayland/server"
	"testing"
)

func TestNilHandlerRejected(t *testing.T) {
	if _, err := NewCompositor(server.Client{}, 1, 1, nil); err == nil {
		t.Fatal("nil handler accepted")
	}
}

func TestForwardReferences(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want interface{}
	}{
		{"create_surface", CompositorInterface.Requests[0].Types[0], SurfaceInterface},
	} {
		if tc.got == nil || tc.got != tc.want {
			t.Fatalf("%s type=%v want %v", tc.name, tc.got, tc.want)
		}
	}
}
