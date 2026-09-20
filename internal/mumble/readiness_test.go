package mumble

import (
	"context"
	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	ma "github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
	"testing"
)

func TestDelayedPeerCannotMutateReusedSession(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	old := &mumble.User{Name: "old"}
	oldRef := connectRemoteTestUser(t, s, old, nil)
	stale := fakeRemotePeer{sessionID: old.SessionID, generation: old.SessionGeneration}
	s.UnregisterConn(oldRef)
	s.users.RemoveIfGeneration(oldRef.SessionID, oldRef.Generation)
	fresh := &mumble.User{Name: "fresh"}
	freshRef := connectRemoteTestUser(t, s, fresh, &recordingSink{})
	if freshRef.SessionID != oldRef.SessionID {
		t.Fatal("未复用 session")
	}
	payload, _ := (&messages.UserState{SelfMute: true, SetFields: messages.UserStateSetSelfMute}).Marshal()
	if err := s.peerAdapt(s.handleUserState)(protocol.MessageUserState, payload, stale); err != nil {
		t.Fatal(err)
	}
	vt, _ := (&messages.VoiceTarget{ID: 1, Targets: []messages.VoiceTargetTarget{{Session: []uint32{freshRef.SessionID}}}}).Marshal()
	if err := s.peerAdapt(s.handleVoiceTarget)(protocol.MessageVoiceTarget, vt, stale); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.users.Snapshot(freshRef.SessionID); u.SelfMute {
		t.Fatal("旧请求修改了新会话")
	}
	if _, ok := s.voiceTargets.Load(freshRef.SessionID); ok {
		t.Fatal("旧请求写入了新会话的 VoiceTarget")
	}
	s.UnregisterConn(oldRef)
	if !s.registry.Active(freshRef) {
		t.Fatal("旧断线事件清理了新会话")
	}
}

func TestVoiceTargetCanonicalKeepsPositionAndGeneration(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	a, b := &mumble.User{Name: "a"}, &mumble.User{Name: "b"}
	ac, _ := connectTestUser(t, s, a)
	connectTestUser(t, s, b)
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{Session: []uint32{b.SessionID}})
	frame := ma.Frame{HasPosition: true, Target: 1, OpusData: []byte{1}}
	got := s.resolveVoiceTargetCanonical(a.SessionID, 1, frame)
	if len(got) != 1 || got[0].Session != userRef(b) || !got[0].Delivery.HasPosition {
		t.Fatalf("canonical 元数据丢失: %+v", got)
	}
}

func TestLocalVoiceRejectsConnectionReplacementAfterValidation(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	a, b := &mumble.User{Name: "a"}, &mumble.User{Name: "b"}
	connectTestUser(t, s, a)
	connectTestUser(t, s, b)
	// 保持旧 registry 为 Active，只替换连接表，确定性模拟校验与查找之间的复用窗口。
	replacement, side := newAuthenticatingConn(t)
	replacement.SetSessionID(b.SessionID)
	replacement.SetSessionGeneration(b.SessionGeneration + 1)
	replacement.SetActive()
	s.connMu.Lock()
	s.conns[b.SessionID] = replacement
	s.connMu.Unlock()
	batch := cluster.VoiceBatch{Sender: userRef(a), Frame: ma.Frame{Codec: ma.CodecOpus, OpusData: []byte{1}}, Recipients: []cluster.VoiceRecipient{{Session: userRef(b), Volume: 1}}}
	if err := (localVoiceTransport{s: s}).DeliverVoice(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	expectNoBroadcast(t, side)
}
