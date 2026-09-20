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
	ErrControlOverflow = errors.New("control queue limit exceeded")
)

type ControlMessage struct {
	Type    protocol.MessageType
	Message messages.Message
}

// ControlSink 的写入只由该会话的 worker 调用；Close 必须可并发调用并中断写入。
type ControlSink interface {
	WriteControl(ControlMessage) error
	Flush(context.Context) error
	Close() error
}

type controlCommand struct {
	message *ControlMessage
	ctx     context.Context
	final   bool
	done    chan error
}

type controlSession struct {
	mu                 sync.Mutex
	sink               ControlSink
	queue              chan controlCommand
	stop               chan struct{}
	stopped            bool
	closed, committing bool
	failed             error
	deferred           []ControlMessage
	limit              int
	onFailure          func(error)
	closeOnce          sync.Once
}

type ControlTransport struct {
	registry  *Registry
	mu        sync.RWMutex
	sessions  map[SessionRef]*controlSession
	onFailure func(SessionRef, error)
}

func NewControlTransport(registry *Registry) *ControlTransport {
	return &ControlTransport{registry: registry, sessions: make(map[SessionRef]*controlSession)}
}

// SetFailureHandler 设置仅作用于失败 generation 的业务清理入口。
func (t *ControlTransport) SetFailureHandler(fn func(SessionRef, error)) {
	t.mu.Lock()
	t.onFailure = fn
	t.mu.Unlock()
}

func (t *ControlTransport) BeginSync(ref SessionRef, sink ControlSink, limit int) error {
	if sink == nil || limit < 1 {
		return ErrInvalidSession
	}
	return t.registry.withTransition(ref, func(snap SessionSnapshot) error {
		if snap.State != StateSyncing {
			return ErrInvalidState
		}
		// 额外一格只保留给 barrier/final，不扩大普通消息的有界容量。
		cs := &controlSession{sink: sink, queue: make(chan controlCommand, limit+1), stop: make(chan struct{}), limit: limit}
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.sessions[ref] != nil {
			return ErrSessionBound
		}
		cs.onFailure = func(err error) {
			t.mu.RLock()
			fn := t.onFailure
			t.mu.RUnlock()
			if fn != nil {
				fn(ref, err)
			} else {
				_, _ = t.registry.BeginClose(ref)
			}
		}
		t.sessions[ref] = cs
		go cs.run()
		return nil
	})
}

// withQueue 在 Registry 读锁内完成状态校验和入队，与所有关闭/解绑原子协调。
// 不使用 transition 锁，避免同时 announce 的会话相互广播时交叉等待。
func (t *ControlTransport) withQueue(ref SessionRef, fn func(SessionState, *controlSession) error) error {
	t.registry.mu.RLock()
	defer t.registry.mu.RUnlock()
	e := t.registry.sessions[ref.SessionID]
	if e == nil || e.ref != ref {
		return ErrStaleSession
	}
	cs := t.session(ref)
	if cs == nil {
		return ErrStaleSession
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return fn(e.state, cs)
}

func (t *ControlTransport) SendInitial(ref SessionRef, message ControlMessage) error {
	message.Message = messages.Clone(message.Message)
	return t.withQueue(ref, func(state SessionState, cs *controlSession) error {
		if cs.closed {
			return ErrControlClosed
		}
		if state != StateSyncing || cs.committing {
			return ErrInvalidState
		}
		return cs.enqueueLocked(controlCommand{message: &message}, false)
	})
}

// Send 只做有界入队，不等待网络。每个会话独立 worker 保证 FIFO。
func (t *ControlTransport) Send(ref SessionRef, message ControlMessage) error {
	message.Message = messages.Clone(message.Message)
	return t.withQueue(ref, func(state SessionState, cs *controlSession) error {
		if cs.closed {
			return ErrControlClosed
		}
		switch state {
		case StateSyncing:
			if len(cs.deferred) >= cs.limit {
				cs.failLocked(ErrControlOverflow)
				return ErrControlOverflow
			}
			cs.deferred = append(cs.deferred, message)
			return nil
		case StateActive:
			return cs.enqueueLocked(controlCommand{message: &message}, false)
		default:
			return ErrInvalidState
		}
	})
}

func (cs *controlSession) enqueueLocked(cmd controlCommand, reserved bool) error {
	if !reserved && len(cs.queue) >= cs.limit {
		cs.failLocked(ErrControlOverflow)
		return ErrControlOverflow
	}
	select {
	case cs.queue <- cmd:
		return nil
	default:
		cs.failLocked(ErrControlOverflow)
		return ErrControlOverflow
	}
}

func (cs *controlSession) stopLocked() {
	cs.closed = true
	cs.deferred = nil
	if !cs.stopped {
		cs.stopped = true
		close(cs.stop)
	}
}

func (cs *controlSession) closeSink() { cs.closeOnce.Do(func() { _ = cs.sink.Close() }) }

func (cs *controlSession) failLocked(err error) {
	if cs.stopped {
		return
	}
	cs.failed = err
	cs.stopLocked()
	// 清理和 sink.Close 均不占用 Core/队列锁，也不互相等待。
	go cs.closeSink()
	go cs.onFailure(err)
}

func (cs *controlSession) run() {
	for {
		select {
		case <-cs.stop:
			return
		default:
		}
		var cmd controlCommand
		select {
		case <-cs.stop:
			return
		case cmd = <-cs.queue:
		}
		select {
		case <-cs.stop:
			return
		default:
		}
		var err error
		if cmd.message != nil {
			err = cs.sink.WriteControl(*cmd.message)
		}
		if err == nil && cmd.ctx != nil {
			err = cs.sink.Flush(cmd.ctx)
		}
		if err != nil {
			cs.mu.Lock()
			cs.failLocked(err)
			cs.mu.Unlock()
		}
		if cmd.final {
			cs.closeSink()
		}
		if cmd.done != nil {
			cmd.done <- err
		}
		if err != nil || cmd.final {
			return
		}
	}
}

func (t *ControlTransport) CommitSync(ctx context.Context, ref SessionRef) error {
	return t.CommitSyncAndAnnounce(ctx, ref, nil)
}

func (t *ControlTransport) CommitSyncAndAnnounce(ctx context.Context, ref SessionRef, announce func()) error {
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	var cs *controlSession
	err := t.withQueue(ref, func(state SessionState, current *controlSession) error {
		cs = current
		if cs.closed || state != StateSyncing || cs.committing {
			return ErrInvalidState
		}
		cs.committing = true
		return cs.enqueueLocked(controlCommand{ctx: ctx, done: done}, true)
	})
	if err != nil {
		return err
	}
	if err = cs.await(ctx, done); err != nil {
		return err
	}
	return t.registry.withTransition(ref, func(snap SessionSnapshot) error {
		t.registry.mu.Lock()
		cs.mu.Lock()
		cs.committing = false
		e := t.registry.sessions[ref.SessionID]
		if e == nil || e.ref != ref || e.state != StateSyncing || cs.closed {
			cs.mu.Unlock()
			t.registry.mu.Unlock()
			return ErrInvalidState
		}
		for i := range cs.deferred {
			message := cs.deferred[i]
			if err := cs.enqueueLocked(controlCommand{message: &message}, false); err != nil {
				cs.mu.Unlock()
				t.registry.mu.Unlock()
				return err
			}
		}
		cs.deferred = nil
		e.state = StateActive
		cs.mu.Unlock()
		t.registry.mu.Unlock()
		if announce != nil {
			announce()
			t.registry.mu.Lock()
			e.announced = true
			t.registry.mu.Unlock()
		}
		return nil
	})
}

func (cs *controlSession) await(ctx context.Context, done <-chan error) error {
	select {
	case err := <-done:
		return err
	case <-cs.stop:
		cs.mu.Lock()
		err := cs.failed
		cs.mu.Unlock()
		if err != nil {
			return err
		}
		return ErrControlClosed
	case <-ctx.Done():
		cs.mu.Lock()
		cs.failLocked(ctx.Err())
		cs.mu.Unlock()
		return ctx.Err()
	}
}

func (t *ControlTransport) AbortSync(ref SessionRef) error {
	cs := t.take(ref)
	if cs == nil {
		return ErrStaleSession
	}
	cs.mu.Lock()
	cs.stopLocked()
	cs.mu.Unlock()
	cs.closeSink()
	return nil
}

func (t *ControlTransport) SendThenClose(ref SessionRef, final ControlMessage, deadline time.Time) error {
	final.Message = messages.Clone(final.Message)
	return t.close(ref, &final, deadline)
}

func (t *ControlTransport) CloseAfterFlush(ref SessionRef, deadline time.Time) error {
	return t.close(ref, nil, deadline)
}

func (t *ControlTransport) close(ref SessionRef, final *ControlMessage, deadline time.Time) error {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	done := make(chan error, 1)
	var cs *controlSession
	err := t.withQueue(ref, func(state SessionState, current *controlSession) error {
		cs = current
		if cs.closed || state == StateClosed {
			return ErrControlClosed
		}
		cs.closed = true
		cs.deferred = nil
		return cs.enqueueLocked(controlCommand{message: final, ctx: ctx, final: true, done: done}, true)
	})
	if err != nil {
		return err
	}
	defer t.take(ref)
	return cs.await(ctx, done)
}

// Detach 中止旧队列，不能留下等待下一条消息的 worker。
func (t *ControlTransport) Detach(ref SessionRef) {
	if cs := t.take(ref); cs != nil {
		cs.mu.Lock()
		cs.stopLocked()
		cs.mu.Unlock()
	}
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
