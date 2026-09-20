package cluster

import (
	"context"
	"errors"
	"sync"
	"testing"

	ma "github.com/dchote/go-mumble-server/pkg/mumble/audio"
)

type fakeVoiceTransport struct {
	mu      sync.Mutex
	batches []VoiceBatch
	err     error
}

func (f *fakeVoiceTransport) DeliverVoice(_ context.Context, batch VoiceBatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.batches = append(f.batches, batch)
	return nil
}

func (f *fakeVoiceTransport) results() []VoiceBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]VoiceBatch(nil), f.batches...)
}

type dispatcherFixture struct {
	registry *Registry
	dispatch *Dispatcher
	local    *fakeVoiceTransport
	edgeA    *fakeVoiceTransport
	edgeB    *fakeVoiceTransport
}

// activeSession binds and activates a session on the given edge.
func activeSession(t *testing.T, f *dispatcherFixture, sid uint32, gen uint64, edge EdgeID) SessionRef {
	t.Helper()
	rref := ref(sid, gen)
	if err := f.registry.Bind(rref, edge); err != nil {
		t.Fatal(err)
	}
	if err := f.registry.CommitActive(rref); err != nil {
		t.Fatal(err)
	}
	return rref
}

func newDispatcherFixture(t *testing.T) *dispatcherFixture {
	t.Helper()
	f := &dispatcherFixture{registry: NewRegistry()}
	for _, e := range []EdgeID{LocalEdgeID, "A", "B"} {
		if err := f.registry.RegisterEdge(e, 1); err != nil {
			t.Fatal(err)
		}
	}
	f.dispatch = NewDispatcher(f.registry)
	f.local, f.edgeA, f.edgeB = &fakeVoiceTransport{}, &fakeVoiceTransport{}, &fakeVoiceTransport{}
	f.dispatch.RegisterVoiceTransport(LocalEdgeID, f.local)
	f.dispatch.RegisterVoiceTransport("A", f.edgeA)
	f.dispatch.RegisterVoiceTransport("B", f.edgeB)
	return f
}

func TestDispatcherGroupsRecipientsByEdge(t *testing.T) {
	f := newDispatcherFixture(t)
	l1 := activeSession(t, f, 1, 1, LocalEdgeID)
	a100 := activeSession(t, f, 100, 1, "A")
	a101 := activeSession(t, f, 101, 1, "A")
	b200 := activeSession(t, f, 200, 1, "B")

	batch := VoiceBatch{
		Sender: l1,
		Frame:  ma.Frame{OpusData: []byte{9}},
		Recipients: []VoiceRecipient{
			{Session: l1, Context: ma.ContextNormal, Volume: 1},
			{Session: a100, Context: ma.ContextWhisper, Volume: 1.5},
			{Session: a101, Context: ma.ContextListen, Volume: 2, HasPosition: true},
			{Session: b200, Context: ma.ContextShout, Volume: 1},
		},
	}
	f.dispatch.DispatchVoice(context.Background(), batch)

	if got := f.local.results(); len(got) != 1 || len(got[0].Recipients) != 1 {
		t.Fatalf("local batches = %d recipients = %d, want 1/1", len(got), len(got[0].Recipients))
	}
	gotA := f.edgeA.results()
	if len(gotA) != 1 {
		t.Fatalf("edge A calls = %d, want single batched call", len(gotA))
	}
	if len(gotA[0].Recipients) != 2 {
		t.Fatalf("edge A recipients = %d, want 100 and 101 in one batch", len(gotA[0].Recipients))
	}
	if gotA[0].Recipients[0].Session != a100 || gotA[0].Recipients[0].Volume != 1.5 {
		t.Fatalf("recipient 100 = %+v", gotA[0].Recipients[0])
	}
	if gotA[0].Recipients[1].Session != a101 || !gotA[0].Recipients[1].HasPosition || gotA[0].Recipients[1].Context != ma.ContextListen {
		t.Fatalf("recipient 101 = %+v", gotA[0].Recipients[1])
	}
	if got := f.edgeB.results(); len(got) != 1 || got[0].Recipients[0].Session != b200 {
		t.Fatalf("edge B delivery = %+v", got)
	}
}

func TestDispatcherDropsIneligibleRecipients(t *testing.T) {
	f := newDispatcherFixture(t)
	sender := activeSession(t, f, 1, 1, LocalEdgeID)
	// Syncing (never committed), unbound, and stale-generation targets.
	_ = activeSession(t, f, 2, 1, LocalEdgeID)
	if err := f.registry.Unbind(ref(2, 1)); err != nil {
		t.Fatal(err)
	}
	if err := f.registry.Bind(ref(3, 1), LocalEdgeID); err != nil {
		t.Fatal(err) // stays Syncing
	}
	stale := ref(100, 7)
	if err := f.registry.Bind(ref(100, 1), "A"); err != nil {
		t.Fatal(err)
	}
	if err := f.registry.CommitActive(ref(100, 1)); err != nil {
		t.Fatal(err)
	}
	_ = stale

	f.dispatch.DispatchVoice(context.Background(), VoiceBatch{
		Sender: sender,
		Recipients: []VoiceRecipient{
			{Session: ref(2, 1)},
			{Session: ref(3, 1)},
			{Session: ref(100, 7)}, // stale generation must not reach session 100
			{Session: ref(999, 1)}, // never existed
		},
	})
	if got := f.local.results(); len(got) != 0 {
		t.Fatalf("local received %v for ineligible recipients", got)
	}
	if got := f.edgeA.results(); len(got) != 0 {
		t.Fatalf("edge A received stale delivery: %v", got)
	}
}

func TestDispatcherClonesFrameOwnership(t *testing.T) {
	f := newDispatcherFixture(t)
	sender := activeSession(t, f, 1, 1, LocalEdgeID)
	away := activeSession(t, f, 100, 1, "A")

	frame := ma.Frame{OpusData: []byte{1, 2, 3}}
	f.dispatch.DispatchVoice(context.Background(), VoiceBatch{
		Sender:     sender,
		Frame:      frame,
		Recipients: []VoiceRecipient{{Session: away}},
	})
	// Mutating the caller's buffer after dispatch must not affect the batch
	// the transport received.
	frame.OpusData[0] = 0xFF
	got := f.edgeA.results()
	if len(got) != 1 || string(got[0].Frame.OpusData) != "\x01\x02\x03" {
		t.Fatalf("frame not cloned: %+v", got)
	}
}

func TestDispatcherIsolatesEdgeFailure(t *testing.T) {
	f := newDispatcherFixture(t)
	sender := activeSession(t, f, 1, 1, LocalEdgeID)
	away := activeSession(t, f, 100, 1, "A")
	other := activeSession(t, f, 200, 1, "B")
	f.edgeA.err = errors.New("edge A offline")

	f.dispatch.DispatchVoice(context.Background(), VoiceBatch{
		Sender:     sender,
		Recipients: []VoiceRecipient{{Session: away}, {Session: other}, {Session: sender}},
	})
	if got := f.edgeB.results(); len(got) != 1 {
		t.Fatalf("edge B skipped after edge A failure: %v", got)
	}
	if got := f.local.results(); len(got) != 1 {
		t.Fatalf("local skipped after edge A failure: %v", got)
	}
}

func TestDispatcherUnregisteredEdgeIsSkipped(t *testing.T) {
	f := newDispatcherFixture(t)
	if err := f.registry.RegisterEdge("C", 1); err != nil {
		t.Fatal(err)
	}
	sender := activeSession(t, f, 1, 1, LocalEdgeID)
	away := activeSession(t, f, 300, 1, "C")
	// No transport registered for C: the recipient is dropped without error.
	f.dispatch.DispatchVoice(context.Background(), VoiceBatch{
		Sender:     sender,
		Recipients: []VoiceRecipient{{Session: away}},
	})
	if got := f.edgeA.results(); len(got) != 0 {
		t.Fatalf("unexpected delivery: %v", got)
	}
}
