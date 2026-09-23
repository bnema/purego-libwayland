package xdgoutput

import "testing"

func TestOutputInterface(t *testing.T) {
	if ZxdgOutputManagerV1Interface == nil || ZxdgOutputV1Interface == nil {
		t.Fatal("xdg-output interfaces not initialized")
	}
	if got := ZxdgOutputManagerV1Interface.Requests[1].Types[0]; got != ZxdgOutputV1Interface {
		t.Fatalf("get_xdg_output new_id type = %v", got)
	}
}
