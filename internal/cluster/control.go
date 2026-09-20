package cluster

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

var (
	ErrControlClosed   = errors.New("control queue is closed")
	ErrControlOverflow = errors.New("control deferred queue limit exceeded")
)

type ControlMessage struct {
	Type    protocol.MessageType
	Message messages.Message
}

// ControlSink is implemented by a local connection or a test remote edge.
// Flush is the write receipt: it succeeds only after prior writes are committed.
type ControlSink interface {
	WriteControl(ControlMessage) error
	Flush(context.Context) error
	Close() error
}

type controlSession struct {
	mu            sync.Mutex
	sink          ControlSink
	syncing       bool
	committing    bool
	closed        bool
	deferred      []ControlMessage
	deferredLimit int
}

// ControlTransport supplies per-session ordering without a global writer lock.
type ControlTransport struct {
	registry *Registry
	mu       sync.RWMutex
	sessions map[SessionRef]*controlSession
}

func NewControlTransport(registry *Registry) *ControlTransport {
	return &ControlTransport{registry: registry, sessions: make(map[SessionRef]*controlSession)}
}

func (t *ControlTransport) BeginSync(ref SessionRef, sink ControlSink, deferredLimit int) error {
	if sink == nil || deferredLimit < 1 {
		return ErrInvalidSession
	}
	snapshot, ok := t.registry.Snapshot(ref)
	if !ok || snapshot.State != StateSyncing {
		return ErrInvalidState
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.sessions[ref]; exists {
		return ErrSessionBound
	}
	t.sessions[ref] = &controlSession{sink: sink, syncing: true, deferredLimit: deferredLimit}
	return nil
}

func (t *ControlTransport) SendInitial(ref SessionRef, message ControlMessage) error {
	message.Message = messages.Clone(message.Message)
	cs := t.session(ref)
	if cs == nil {
		return ErrStaleSession
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.closed {
		return ErrControlClosed
	}
	if !cs.syncing || cs.committing {
		return ErrInvalidState
	}
	return cs.sink.WriteControl(message)
}

// Send writes for Active sessions and defers ordinary broadcasts for Syncing
// sessions. 入队前深拷贝消息，调用方保留原对象的所有权。
func (t *ControlTransport) Send(ref SessionRef, message ControlMessage) error {
	message.Message = messages.Clone(message.Message)
	cs := t.session(ref)
	if cs == nil {
		return ErrStaleSession
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.closed {
		return ErrControlClosed
	}
	if cs.syncing {
		if len(cs.deferred) >= cs.deferredLimit {
			cs.closed = true
			return ErrControlOverflow
		}
		cs.deferred = append(cs.deferred, message)
		return nil
	}
	if !t.registry.Active(ref) {
		return ErrInvalidState
	}
	return cs.sink.WriteControl(message)
}

func (t *ControlTransport) CommitSync(ctx context.Context, ref SessionRef) error {
	return t.CommitSyncAndAnnounce(ctx, ref, nil)
}

func (t *ControlTransport) CommitSyncAndAnnounce(ctx context.Context, ref SessionRef, announce func()) error {
	cs := t.session(ref)
	if cs == nil {
		return ErrStaleSession
	}
	cs.mu.Lock()
	locked := true
	defer func() {
		if locked {
			cs.mu.Unlock()
		}
	}()
	if cs.closed || !cs.syncing || cs.committing {
		return ErrInvalidState
	}
	cs.committing = true
	cs.mu.Unlock()
	// 等待物理写入时，普通广播仍能进入本会话的 deferred 队列。
	flushCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	flushErr := cs.sink.Flush(flushCtx)
	cancel()
	cs.mu.Lock()
	cs.committing = false
	if flushErr != nil {
		cs.closed = true
		return flushErr
	}
	if cs.closed {
		return ErrControlClosed
	}
	for _, message := range cs.deferred {
		if err := cs.sink.WriteControl(message); err != nil {
			cs.closed = true
			return err
		}
	}
	cs.deferred = nil
	commit := t.registry.CommitActiveAndAnnounce
	if announce == nil {
		commit = func(ref SessionRef, done func()) error {
			err := t.registry.CommitActive(ref)
			if err == nil {
				done()
			}
			return err
		}
	}
	if err := commit(ref, func() {
		cs.syncing = false
		cs.mu.Unlock()
		locked = false
		if announce != nil {
			announce()
		}
	}); err != nil {
		cs.closed = true
		return err
	}
	return nil
}

func (t *ControlTransport) AbortSync(ref SessionRef) error {
	cs := t.take(ref)
	if cs == nil {
		return ErrStaleSession
	}
	cs.mu.Lock()
	cs.closed = true
	cs.deferred = nil
	cs.mu.Unlock()
	return cs.sink.Close()
}

func (t *ControlTransport) SendThenClose(ref SessionRef, final ControlMessage, deadline time.Time) error {
	final.Message = messages.Clone(final.Message)
	cs := t.take(ref)
	if cs == nil {
		return ErrStaleSession
	}
	cs.mu.Lock()
	if cs.closed {
		cs.mu.Unlock()
		return ErrControlClosed
	}
	cs.closed = true
	err := cs.sink.WriteControl(final)
	if err == nil {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		err = cs.sink.Flush(ctx)
		cancel()
	}
	closeErr := cs.sink.Close()
	cs.mu.Unlock()
	if err != nil {
		return err
	}
	return closeErr
}

func (t *ControlTransport) CloseAfterFlush(ref SessionRef, deadline time.Time) error {
	cs := t.take(ref)
	if cs == nil {
		return ErrStaleSession
	}
	cs.mu.Lock()
	cs.closed = true
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	err := cs.sink.Flush(ctx)
	cancel()
	closeErr := cs.sink.Close()
	cs.mu.Unlock()
	if err != nil {
		return err
	}
	return closeErr
}

// Detach removes a session's control queue without closing its sink. The
// disconnect cleanup path uses it when the connection teardown is already
// owned elsewhere (SendThenClose, the read loop, or test cleanup).
func (t *ControlTransport) Detach(ref SessionRef) {
	t.take(ref)
}

func (t *ControlTransport) session(ref SessionRef) *controlSession {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.sessions[ref]
}

func (t *ControlTransport) take(ref SessionRef) *controlSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	cs := t.sessions[ref]
	delete(t.sessions, ref)
	return cs
}
