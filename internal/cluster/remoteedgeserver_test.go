package cluster

import (
	"context"
	"testing"
)

// The Phase 2A skeleton: the capability binds the real registry, starts and
// closes cleanly in both disabled and configured states, and never disturbs
// the registry it will later serve.
func TestRemoteEdgeServerSkeleton(t *testing.T) {
	reg := NewRegistry()

	disabled := NewRemoteEdgeServer(reg, "")
	if disabled.Registry() != reg {
		t.Fatal("capability is not bound to the given registry")
	}
	if disabled.ListenAddr() != "" {
		t.Fatalf("ListenAddr = %q, want empty", disabled.ListenAddr())
	}
	if err := disabled.Start(context.Background()); err != nil {
		t.Fatalf("Start (disabled): %v", err)
	}
	if err := disabled.Close(); err != nil {
		t.Fatalf("Close (disabled): %v", err)
	}

	configured := NewRemoteEdgeServer(reg, "127.0.0.1:64740")
	if configured.ListenAddr() != "127.0.0.1:64740" {
		t.Fatalf("ListenAddr = %q", configured.ListenAddr())
	}
	if err := configured.Start(context.Background()); err != nil {
		t.Fatalf("Start (configured): %v", err)
	}
	if err := configured.Close(); err != nil {
		t.Fatalf("Close (configured): %v", err)
	}

	// The registry stays fully usable across capability lifecycle calls.
	if err := reg.RegisterEdge("probe-edge", 1); err != nil {
		t.Fatalf("registry unusable after capability start/close: %v", err)
	}
	if !reg.EdgeRegistered("probe-edge") {
		t.Fatal("EdgeRegistered disagrees with RegisterEdge")
	}
}
