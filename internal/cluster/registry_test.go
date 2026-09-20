package cluster

import (
	"errors"
	"sync"
	"testing"
)

func mustEdge(t *testing.T, r *Registry, id EdgeID, gen uint64) {
	t.Helper()
	if err := r.RegisterEdge(id, gen); err != nil {
		t.Fatalf("RegisterEdge(%s): %v", id, err)
	}
}

func ref(sid uint32, gen uint64) SessionRef {
	return SessionRef{SessionID: sid, Generation: gen}
}

func TestRegistryBindRequiresRegisteredEdge(t *testing.T) {
	r := NewRegistry()
	if err := r.Bind(ref(1, 1), "nowhere"); !errors.Is(err, ErrEdgeNotFound) {
		t.Fatalf("Bind to unknown edge = %v, want ErrEdgeNotFound", err)
	}
	if err := r.Bind(SessionRef{}, LocalEdgeID); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("Bind zero ref = %v, want ErrInvalidSession", err)
	}
}

func TestRegistryBindLifecycle(t *testing.T) {
	r := NewRegistry()
	mustEdge(t, r, LocalEdgeID, 1)
	a := ref(1, 10)
	if err := r.Bind(a, LocalEdgeID); err != nil {
		t.Fatal(err)
	}
	// Same binding is idempotent.
	if err := r.Bind(a, LocalEdgeID); err != nil {
		t.Fatalf("idempotent rebind: %v", err)
	}
	// A different generation on the same session ID is refused until unbound.
	if err := r.Bind(ref(1, 11), LocalEdgeID); !errors.Is(err, ErrSessionBound) {
		t.Fatalf("conflicting bind = %v, want ErrSessionBound", err)
	}

	if s, ok := r.Snapshot(a); !ok || s.State != StateSyncing || s.Edge != LocalEdgeID {
		t.Fatalf("snapshot after bind = %+v ok=%v, want Syncing on local", s, ok)
	}
	if r.Active(a) {
		t.Fatal("syncing session must not be Active")
	}
	if err := r.CommitActive(a); err != nil {
		t.Fatal(err)
	}
	if !r.Active(a) {
		t.Fatal("session not active after commit")
	}
	if err := r.CommitActive(a); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("double commit = %v, want ErrInvalidState", err)
	}

	if snap, err := r.BeginClose(a); err != nil || snap.State != StateClosing {
		t.Fatalf("BeginClose = %+v %v, want Closing", snap, err)
	}
	// Closing twice is a no-op.
	if _, err := r.BeginClose(a); err != nil {
		t.Fatalf("second BeginClose: %v", err)
	}
	if err := r.Unbind(a); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Snapshot(a); ok {
		t.Fatal("session survived unbind")
	}
}

func TestRegistrySessionIDReuse(t *testing.T) {
	r := NewRegistry()
	mustEdge(t, r, LocalEdgeID, 1)
	old := ref(7, 1)
	if err := r.Bind(old, LocalEdgeID); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitActive(old); err != nil {
		t.Fatal(err)
	}
	// Stale generation operations must not affect the entry.
	if err := r.CommitActive(ref(7, 2)); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("stale commit = %v, want ErrStaleSession", err)
	}
	if err := r.Unbind(ref(7, 99)); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("stale unbind = %v, want ErrStaleSession", err)
	}
	if err := r.Unbind(old); err != nil {
		t.Fatal(err)
	}
	// The ID is reusable with a new generation.
	fresh := ref(7, 2)
	if err := r.Bind(fresh, LocalEdgeID); err != nil {
		t.Fatalf("rebind reused ID: %v", err)
	}
	if s, ok := r.Snapshot(fresh); !ok || s.State != StateSyncing {
		t.Fatalf("fresh session = %+v ok=%v", s, ok)
	}
}

func TestRegistryRemoveEdgeCollectsSessions(t *testing.T) {
	r := NewRegistry()
	mustEdge(t, r, "A", 5)
	mustEdge(t, r, LocalEdgeID, 1)
	for _, rref := range []SessionRef{ref(1, 1), ref(2, 1)} {
		if err := r.Bind(rref, "A"); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Bind(ref(3, 1), LocalEdgeID); err != nil {
		t.Fatal(err)
	}
	// A stale edge instance cannot remove the new instance's sessions.
	if removed := r.RemoveEdge("A", 4); removed != nil {
		t.Fatalf("stale RemoveEdge removed %v", removed)
	}
	removed := r.RemoveEdge("A", 5)
	if len(removed) != 2 {
		t.Fatalf("RemoveEdge returned %v, want 2 sessions", removed)
	}
	if _, ok := r.Ref(1); ok {
		t.Fatal("edge session survived edge removal")
	}
	if _, ok := r.Ref(3); !ok {
		t.Fatal("local session removed by foreign edge removal")
	}
}

func TestRegistryEdgeGenerationConflict(t *testing.T) {
	r := NewRegistry()
	mustEdge(t, r, "A", 1)
	mustEdge(t, r, "A", 1) // idempotent
	if err := r.RegisterEdge("A", 2); !errors.Is(err, ErrSessionBound) {
		t.Fatalf("instance conflict = %v, want ErrSessionBound", err)
	}
}

func TestRegistryConcurrentTransitionRace(t *testing.T) {
	r := NewRegistry()
	mustEdge(t, r, LocalEdgeID, 1)
	a := ref(1, 1)
	if err := r.Bind(a, LocalEdgeID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = r.CommitActive(a)
	}()
	go func() {
		defer wg.Done()
		_, _ = r.BeginClose(a)
	}()
	wg.Wait()
	s, ok := r.Snapshot(a)
	if !ok {
		t.Fatal("session vanished under racing transitions")
	}
	if s.State != StateActive && s.State != StateClosing {
		t.Fatalf("race produced invalid state %d", s.State)
	}
}
