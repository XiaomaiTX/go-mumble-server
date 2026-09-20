package cluster

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestClosingRejectsAllControlLanes(t *testing.T) {
	for _, active := range []bool{false, true} {
		tr, registry, sink, session := newSyncingTransport(t)
		if active {
			if err := tr.CommitSync(context.Background(), session); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := registry.BeginClose(session); err != nil {
			t.Fatal(err)
		}
		if err := tr.Send(session, msg(1)); err == nil {
			t.Fatal("Closing 接受了 Send")
		}
		if err := tr.SendInitial(session, msg(2)); err == nil {
			t.Fatal("Closing 接受了 SendInitial")
		}
		if err := tr.CommitSync(context.Background(), session); err == nil {
			t.Fatal("Closing 接受了 CommitSync")
		}
		if err := tr.SendThenClose(session, msg(3), time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if got := ids(sink.snapshot()); !equalIDs(got, 3) {
			t.Fatalf("关闭消息序列: %v", got)
		}
	}
}

func TestCloseSendCommitRaceAndReuse(t *testing.T) {
	for i := 0; i < 100; i++ {
		tr, registry, _, old := newSyncingTransport(t)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, fn := range []func(){
			func() { _ = tr.Send(old, msg(1)) },
			func() { _ = tr.SendInitial(old, msg(2)) },
			func() { _ = tr.CommitSync(context.Background(), old) },
			func() { _, _ = registry.BeginClose(old) },
		} {
			wg.Add(1)
			go func(fn func()) { defer wg.Done(); <-start; fn() }(fn)
		}
		close(start)
		wg.Wait()
		if err := tr.Send(old, msg(3)); err == nil {
			t.Fatal("关闭后仍接受消息")
		}
		if err := registry.Unbind(old); err != nil {
			t.Fatal(err)
		}
		fresh := ref(old.SessionID, old.Generation+1)
		sink := &fakeSink{}
		if err := registry.Bind(fresh, LocalEdgeID); err != nil {
			t.Fatal(err)
		}
		if err := tr.BeginSync(fresh, sink, 8); err != nil {
			t.Fatal(err)
		}
		if err := tr.CommitSync(context.Background(), fresh); err != nil {
			t.Fatal(err)
		}
		if err := tr.Send(old, msg(9)); !errors.Is(err, ErrStaleSession) {
			t.Fatal(err)
		}
		if err := tr.SendInitial(old, msg(9)); !errors.Is(err, ErrStaleSession) {
			t.Fatal(err)
		}
		if err := tr.CommitSync(context.Background(), old); !errors.Is(err, ErrStaleSession) {
			t.Fatal(err)
		}
		if err := tr.SendThenClose(old, msg(9), time.Now().Add(time.Second)); !errors.Is(err, ErrStaleSession) {
			t.Fatal(err)
		}
		_ = tr.AbortSync(old)
		if !registry.Active(fresh) {
			t.Fatal("迟到操作影响新 generation")
		}
		if err := tr.SendThenClose(fresh, msg(10), time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if got := ids(sink.snapshot()); !equalIDs(got, 10) {
			t.Fatal(got)
		}
	}
}

type blockedControlSink struct {
	fakeSink
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockedControlSink) WriteControl(m ControlMessage) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.fakeSink.WriteControl(m)
}

func TestBlockedWriterIsolationAndOverflow(t *testing.T) {
	r := NewRegistry()
	tr := NewControlTransport(r)
	blocked := &blockedControlSink{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(blocked.release)
	fast, local := &fakeSink{}, &fakeSink{}
	for i, item := range []struct {
		edge EdgeID
		sink ControlSink
	}{{"A", blocked}, {"B", fast}, {LocalEdgeID, local}} {
		_ = r.RegisterEdge(item.edge, 1)
		session := ref(uint32(i+1), 1)
		_ = r.Bind(session, item.edge)
		if err := tr.BeginSync(session, item.sink, 4); err != nil {
			t.Fatal(err)
		}
		if err := tr.CommitSync(context.Background(), session); err != nil {
			t.Fatal(err)
		}
		defer tr.AbortSync(session)
	}
	if err := tr.Send(ref(1, 1), msg(0)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("worker 未进入阻塞写入")
	}
	for i := 1; i <= 4; i++ {
		if err := tr.Send(ref(1, 1), msg(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Send(ref(1, 1), msg(5)); !errors.Is(err, ErrControlOverflow) {
		t.Fatalf("overflow: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if err := tr.Send(ref(2, 1), msg(i)); err != nil {
			t.Fatal(err)
		}
		if err := tr.Send(ref(3, 1), msg(i)); err != nil {
			t.Fatal(err)
		}
	}
	waitWrites(t, fast, 3)
	waitWrites(t, local, 3)
	if !equalIDs(ids(fast.snapshot()), 1, 2, 3) || !equalIDs(ids(local.snapshot()), 1, 2, 3) {
		t.Fatal("FIFO 失序")
	}
	deadline := time.Now().Add(time.Second)
	for r.Active(ref(1, 1)) {
		if time.Now().After(deadline) {
			t.Fatal("溢出未进入关闭路径")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestClosingDuringBlockedCommit(t *testing.T) {
	tr, registry, sink, session := newSyncingTransport(t)
	release := sink.gateFlush()
	done := make(chan error, 1)
	go func() { done <- tr.CommitSync(context.Background(), session) }()
	deadline := time.Now().Add(time.Second)
	for sink.flushCalls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("未进入 barrier")
		}
		time.Sleep(time.Millisecond)
	}
	_, _ = registry.BeginClose(session)
	release()
	if err := <-done; err == nil {
		t.Fatal("关闭后的迟到回执提交了 Active")
	}
	if registry.Active(session) {
		t.Fatal("关闭会话被重新激活")
	}
}
