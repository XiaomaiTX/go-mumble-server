package mumble

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dchote/go-mumble-server/internal/cluster"
	pkgmumble "github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

type stalledEdgeSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *stalledEdgeSink) WriteControl(cluster.ControlMessage) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return errors.New("edge disconnected")
}
func (*stalledEdgeSink) Flush(context.Context) error { return nil }

// 故意不释放 WriteControl，模拟整个测试期间永久阻塞的第三方 sink。
func (*stalledEdgeSink) Close() error { return nil }

type notifyingEdgeSink struct{ delivered chan cluster.ControlMessage }

func (s *notifyingEdgeSink) WriteControl(m cluster.ControlMessage) error {
	s.delivered <- m
	return nil
}
func (*notifyingEdgeSink) Flush(context.Context) error { return nil }
func (*notifyingEdgeSink) Close() error                { return nil }

func TestBroadcastBlockedEdgeDoesNotBlockOtherEdgeOrLocal(t *testing.T) {
	s := newACLTestServer(t)
	stalled := &stalledEdgeSink{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(stalled.release)
	fast := &notifyingEdgeSink{delivered: make(chan cluster.ControlMessage, 32)}
	add := func(name string, edge cluster.EdgeID, sink cluster.ControlSink) cluster.SessionRef {
		u, ok := s.users.Add(pkgmumble.User{Name: name})
		if !ok {
			t.Fatal("add failed")
		}
		ref := cluster.SessionRef{SessionID: u.SessionID, Generation: u.SessionGeneration}
		if err := s.registry.RegisterEdge(edge, 1); err != nil {
			t.Fatal(err)
		}
		if err := s.registry.Bind(ref, edge); err != nil {
			t.Fatal(err)
		}
		if err := s.control.BeginSync(ref, sink, 4); err != nil {
			t.Fatal(err)
		}
		if err := s.control.CommitSync(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.control.Detach(ref) })
		return ref
	}
	a := add("edge-a", "A", stalled)
	b := add("edge-b", "B", fast)
	local := &pkgmumble.User{Name: "local"}
	_, side := connectTestUser(t, s, local)
	for i := 0; i < 8; i++ {
		text := &messages.TextMessage{Message: "control", Actor: uint32(i)}
		done := make(chan struct{})
		go func() { s.Broadcast(0, protocol.MessageTextMessage, text); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Broadcast 被 Edge A 阻塞")
		}
		select {
		case m := <-fast.delivered:
			if m.Type != protocol.MessageTextMessage || m.Message.(*messages.TextMessage).Actor != uint32(i) {
				t.Fatalf("Edge B FIFO 错误: %+v", m)
			}
		case <-time.After(time.Second):
			t.Fatal("Edge B 未收到控制消息")
		}
		kind, data := readMessage(t, side)
		var received messages.TextMessage
		if err := received.Unmarshal(data); err != nil {
			t.Fatal(err)
		}
		if kind != protocol.MessageTextMessage || received.Actor != uint32(i) {
			t.Fatalf("Local FIFO 错误: %+v", received)
		}
		if i == 0 {
			select {
			case <-stalled.entered:
			case <-time.After(time.Second):
				t.Fatal("Edge A 未阻塞")
			}
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, exists := s.users.SnapshotRef(a)
		if !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("溢出后未清理失败会话")
		}
		time.Sleep(time.Millisecond)
	}
	if !s.registry.Active(b) || !s.registry.Active(userRef(local)) {
		t.Fatal("健康会话受到失败会话影响")
	}
}
