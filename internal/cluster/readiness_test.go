package cluster

import (
	"context"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
	"testing"
	"time"
)

func TestActiveAnnouncementSerializesClose(t *testing.T) {
	tr, registry, _, ref := newSyncingTransport(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- tr.CommitSyncAndAnnounce(context.Background(), ref, func() { close(entered); <-release })
	}()
	<-entered
	if snap, _ := registry.Snapshot(ref); snap.Announced {
		t.Fatal("加入广播尚未提交就被标记为 announced")
	}
	closed := make(chan SessionSnapshot, 1)
	go func() { snap, _ := registry.BeginClose(ref); closed <- snap }()
	select {
	case <-closed:
		t.Fatal("关闭越过了尚未提交的加入广播")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if snap := <-closed; !snap.Announced {
		t.Fatal("关闭未观察到已提交的加入广播")
	}
	if snap, _ := registry.BeginClose(ref); snap.Announced {
		t.Fatal("重复关闭不应重复宣布离开")
	}
}

func TestControlPayloadOwnership(t *testing.T) {
	for _, lane := range []string{"initial", "deferred", "active", "final"} {
		t.Run(lane, func(t *testing.T) {
			tr, _, sink, ref := newSyncingTransport(t)
			message := &messages.TextMessage{Message: "原始内容", Session: []uint32{7, 8}}
			frame := ControlMessage{Type: protocol.MessageTextMessage, Message: message}
			var err error
			switch lane {
			case "initial":
				err = tr.SendInitial(ref, frame)
			case "deferred":
				err = tr.Send(ref, frame)
			case "active":
				if err = tr.CommitSync(context.Background(), ref); err == nil {
					err = tr.Send(ref, frame)
				}
			case "final":
				err = tr.SendThenClose(ref, frame, time.Now().Add(time.Second))
			}
			if err != nil {
				t.Fatal(err)
			}
			message.Message = "调用方已复用"
			message.Session[0] = 99
			if lane == "deferred" {
				if err := tr.CommitSync(context.Background(), ref); err != nil {
					t.Fatal(err)
				}
			}
			got := sink.snapshot()[0].Message.(*messages.TextMessage)
			if got.Message != "原始内容" || got.Session[0] != 7 {
				t.Fatalf("队列引用了调用方内存: %+v", got)
			}
		})
	}
}

func TestDispatcherDropsReusedSenderGeneration(t *testing.T) {
	f := newDispatcherFixture(t)
	old := activeSession(t, f, 42, 10, LocalEdgeID)
	recipient := activeSession(t, f, 100, 1, "A")
	if err := f.registry.Unbind(old); err != nil {
		t.Fatal(err)
	}
	activeSession(t, f, 42, 11, LocalEdgeID)
	f.dispatch.DispatchVoice(context.Background(), VoiceBatch{Sender: old, Recipients: []VoiceRecipient{{Session: recipient}}})
	if len(f.edgeA.results()) != 0 {
		t.Fatal("旧 sender generation 的语音被发送到 Edge")
	}
}

func TestBroadcastCanDeferWhileInitialFlushBlocked(t *testing.T) {
	tr, _, sink, ref := newSyncingTransport(t)
	release := sink.gateFlush()
	done := make(chan error, 1)
	go func() { done <- tr.CommitSync(context.Background(), ref) }()
	deadline := time.Now().Add(time.Second)
	for sink.flushCalls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("未进入 flush")
		}
		time.Sleep(time.Millisecond)
	}
	sent := make(chan error, 1)
	go func() { sent <- tr.Send(ref, msg(1)) }()
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		release()
		t.Fatal("慢会话的 flush 阻塞了广播入队")
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
