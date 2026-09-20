package cluster

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
)

// fakeSink records writes; flush can be gated to simulate a slow client.
type fakeSink struct {
	mu         sync.Mutex
	written    []ControlMessage
	flushCalls atomic.Int64
	flushGate  atomic.Pointer[chan struct{}]
	flushErr   atomic.Pointer[error]
	closed     bool
}

func (f *fakeSink) WriteControl(m ControlMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrControlClosed
	}
	f.written = append(f.written, m)
	return nil
}

func (f *fakeSink) Flush(ctx context.Context) error {
	f.flushCalls.Add(1)
	if gate := f.flushGate.Load(); gate != nil {
		select {
		case <-*gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := f.flushErr.Load(); err != nil {
		return *err
	}
	return nil
}

func (f *fakeSink) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeSink) snapshot() []ControlMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ControlMessage(nil), f.written...)
}

func (f *fakeSink) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeSink) gateFlush() (release func()) {
	ch := make(chan struct{})
	f.flushGate.Store(&ch)
	return func() { close(ch) }
}

type stubMessage struct{ id int }

func (stubMessage) Marshal() ([]byte, error) { return nil, nil }
func (stubMessage) Unmarshal([]byte) error   { return nil }

func msg(id int) ControlMessage {
	return ControlMessage{Type: protocol.MessageUserState, Message: stubMessage{id: id}}
}

func newSyncingTransport(t *testing.T) (*ControlTransport, *Registry, *fakeSink, SessionRef) {
	t.Helper()
	r := NewRegistry()
	if err := r.RegisterEdge(LocalEdgeID, 1); err != nil {
		t.Fatal(err)
	}
	tr := NewControlTransport(r)
	sink := &fakeSink{}
	a := ref(1, 1)
	if err := r.Bind(a, LocalEdgeID); err != nil {
		t.Fatal(err)
	}
	if err := tr.BeginSync(a, sink, 8); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.AbortSync(a) })
	return tr, r, sink, a
}

func waitWrites(t *testing.T, sink *fakeSink, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(sink.snapshot()) < count {
		if time.Now().After(deadline) {
			t.Fatalf("写入未完成: got %d, want %d", len(sink.snapshot()), count)
		}
		time.Sleep(time.Millisecond)
	}
}

func ids(cms []ControlMessage) []int {
	out := make([]int, len(cms))
	for i, cm := range cms {
		out[i] = cm.Message.(stubMessage).id
	}
	return out
}

func equalIDs(got []int, want ...int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestControlTransportDeferredSpliceOrder(t *testing.T) {
	tr, r, sink, a := newSyncingTransport(t)

	for i := 1; i <= 3; i++ {
		if err := tr.SendInitial(a, msg(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Ordinary broadcasts while Syncing must defer, not write.
	if err := tr.Send(a, msg(10)); err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(a, msg(11)); err != nil {
		t.Fatal(err)
	}
	waitWrites(t, sink, 3)
	if got := ids(sink.snapshot()); !equalIDs(got, 1, 2, 3) {
		t.Fatalf("pre-commit writes = %v, want initial lane only", got)
	}
	if err := tr.CommitSync(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	waitWrites(t, sink, 5)
	if got := ids(sink.snapshot()); !equalIDs(got, 1, 2, 3, 10, 11) {
		t.Fatalf("post-commit writes = %v, want deferred after initial", got)
	}
	if !r.Active(a) {
		t.Fatal("CommitSync did not commit Active")
	}
	// Post-commit sends go straight to the FIFO tail.
	if err := tr.Send(a, msg(12)); err != nil {
		t.Fatal(err)
	}
	waitWrites(t, sink, 6)
	if got := ids(sink.snapshot()); !equalIDs(got, 1, 2, 3, 10, 11, 12) {
		t.Fatalf("post-active writes = %v", got)
	}
}

func TestControlTransportBarrierWaitsForWriteReceipt(t *testing.T) {
	tr, _, sink, a := newSyncingTransport(t)
	if err := tr.SendInitial(a, msg(1)); err != nil {
		t.Fatal(err)
	}
	release := sink.gateFlush()
	committed := make(chan error, 1)
	go func() { committed <- tr.CommitSync(context.Background(), a) }()
	// While the barrier is unconfirmed the deferred queue must not splice.
	if err := tr.Send(a, msg(9)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := ids(sink.snapshot()); !equalIDs(got, 1) {
		t.Fatalf("deferred spliced before write receipt: %v", got)
	}
	release()
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	waitWrites(t, sink, 2)
	if got := ids(sink.snapshot()); !equalIDs(got, 1, 9) {
		t.Fatalf("post-barrier writes = %v", got)
	}
}

func TestControlTransportDeferredOverflowFailsSync(t *testing.T) {
	tr, r, sink, a := newSyncingTransport(t)
	for i := 0; i < 8; i++ {
		if err := tr.Send(a, msg(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Send(a, msg(99)); !errors.Is(err, ErrControlOverflow) {
		t.Fatalf("overflow = %v, want ErrControlOverflow", err)
	}
	// The queue is closed: no further traffic of any kind.
	if err := tr.Send(a, msg(100)); !errors.Is(err, ErrControlClosed) {
		t.Fatalf("send after overflow = %v, want ErrControlClosed", err)
	}
	if err := tr.SendInitial(a, msg(101)); !errors.Is(err, ErrControlClosed) {
		t.Fatalf("initial after overflow = %v, want ErrControlClosed", err)
	}
	if err := tr.CommitSync(context.Background(), a); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("commit after overflow = %v, want ErrInvalidState", err)
	}
	if r.Active(a) {
		t.Fatal("overflowed session became Active")
	}
	// AbortSync still tears down the closed queues.
	if err := tr.AbortSync(a); err != nil {
		t.Fatal(err)
	}
	if !sink.isClosed() {
		t.Fatal("AbortSync did not close the sink")
	}
}

func TestControlTransportSendThenCloseOrdering(t *testing.T) {
	tr, _, sink, a := newSyncingTransport(t)
	if err := tr.CommitSync(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(a, msg(1)); err != nil {
		t.Fatal(err)
	}
	if err := tr.SendThenClose(a, msg(2), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := ids(sink.snapshot()); !equalIDs(got, 1, 2) {
		t.Fatalf("final sequence = %v, want final last", got)
	}
	if !sink.isClosed() {
		t.Fatal("SendThenClose did not close")
	}
	if err := tr.Send(a, msg(3)); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("send after SendThenClose = %v, want ErrStaleSession", err)
	}
}

func TestControlTransportCloseAfterFlushRejectsNewSends(t *testing.T) {
	tr, _, sink, a := newSyncingTransport(t)
	if err := tr.CommitSync(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(a, msg(1)); err != nil {
		t.Fatal(err)
	}
	if err := tr.CloseAfterFlush(a, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if !sink.isClosed() {
		t.Fatal("CloseAfterFlush did not close")
	}
	if err := tr.Send(a, msg(2)); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("send after close = %v, want ErrStaleSession", err)
	}
}

func TestControlTransportStaleRefRejected(t *testing.T) {
	tr, _, _, a := newSyncingTransport(t)
	stale := ref(1, 2) // same ID, wrong generation
	if err := tr.Send(stale, msg(1)); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("stale send = %v, want ErrStaleSession", err)
	}
	if err := tr.SendInitial(stale, msg(1)); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("stale initial = %v, want ErrStaleSession", err)
	}
	if err := tr.SendThenClose(stale, msg(1), time.Now()); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("stale sendThenClose = %v, want ErrStaleSession", err)
	}
	if err := tr.CommitSync(context.Background(), stale); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("stale commit = %v, want ErrStaleSession", err)
	}
	// Wrong lifecycle: Active session cannot BeginSync again.
	if err := tr.CommitSync(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := tr.BeginSync(a, &fakeSink{}, 8); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("BeginSync on Active = %v, want ErrInvalidState", err)
	}
}

func TestControlTransportSessionsAreIndependent(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterEdge(LocalEdgeID, 1); err != nil {
		t.Fatal(err)
	}
	tr := NewControlTransport(r)
	fast, slow := &fakeSink{}, &fakeSink{}
	fa, fs := ref(1, 1), ref(2, 1)
	for _, s := range []struct {
		ref  SessionRef
		sink *fakeSink
	}{{fa, fast}, {fs, slow}} {
		if err := r.Bind(s.ref, LocalEdgeID); err != nil {
			t.Fatal(err)
		}
		if err := tr.BeginSync(s.ref, s.sink, 8); err != nil {
			t.Fatal(err)
		}
	}
	release := slow.gateFlush()
	done := make(chan error, 1)
	go func() { done <- tr.CommitSync(context.Background(), fs) }()
	// The slow session's barrier must not block the fast session.
	if err := tr.CommitSync(context.Background(), fa); err != nil {
		t.Fatalf("fast session blocked by slow session: %v", err)
	}
	if err := tr.Send(fa, msg(1)); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestControlTransportFlushFailureBlocksCommit(t *testing.T) {
	tr, r, sink, a := newSyncingTransport(t)
	failure := errors.New("socket gone")
	sink.flushErr.Store(&failure)
	if err := tr.CommitSync(context.Background(), a); !errors.Is(err, failure) {
		t.Fatalf("commit = %v, want flush failure", err)
	}
	if r.Active(a) {
		t.Fatal("failed barrier still committed Active")
	}
	// The queue is closed after a failed commit; cleanup must use AbortSync.
	if err := tr.Send(a, msg(1)); !errors.Is(err, ErrControlClosed) {
		t.Fatalf("send after failed commit = %v, want ErrControlClosed", err)
	}
	if err := tr.AbortSync(a); err != nil {
		t.Fatal(err)
	}
}
