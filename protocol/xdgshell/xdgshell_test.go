package xdgshell

import "testing"

func TestForwardReference(t *testing.T) {
	if got := WmBaseInterface.Requests[2].Types[0]; got == nil || got != SurfaceInterface {
		t.Fatalf("get_xdg_surface type=%v want %v", got, SurfaceInterface)
	}
}
