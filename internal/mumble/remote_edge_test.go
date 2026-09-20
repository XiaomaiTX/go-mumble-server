package mumble

// Remote-edge readiness tests: every "remote" session here lives on a
// non-local edge with no net.Conn, no CryptState and no UDP endpoint — the
// exact shape a real remote edge session will have. If a Core path only works
// for sessions with a local conn, these tests fail.

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	ma "github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

const fakeRemoteEdge = cluster.EdgeID("remote-edge")

// fakeRemotePeer is a Peer for a session with no local connection.
type fakeRemotePeer struct {
	sessionID  uint32
	generation uint64
}

func (p fakeRemotePeer) SessionGeneration() uint64 { return p.generation }
func (p fakeRemotePeer) SessionID() uint32         { return p.sessionID }
func (p fakeRemotePeer) WriteMessage(protocol.MessageType, messages.Message) error {
	return nil
}

// recordingSink is a ControlSink that records delivered control messages.
type recordingSink struct {
	mu       sync.Mutex
	messages []cluster.ControlMessage
}

func (r *recordingSink) WriteControl(m cluster.ControlMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, m)
	return nil
}

func (r *recordingSink) Flush(context.Context) error { return nil }
func (r *recordingSink) Close() error                { return nil }

func (r *recordingSink) delivered() []cluster.ControlMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cluster.ControlMessage(nil), r.messages...)
}

// connectRemoteTestUser creates an Active session on a non-local edge. sink may
// be nil, in which case the session commits Active without a control queue.
func connectRemoteTestUser(t *testing.T, srv *Server, u *mumble.User, sink cluster.ControlSink) cluster.SessionRef {
	t.Helper()
	stored, ok := srv.users.Add(*u)
	if !ok {
		t.Fatalf("could not add user %s", u.Name)
	}
	*u = stored
	if err := srv.registry.RegisterEdge(fakeRemoteEdge, 1); err != nil {
		t.Fatal(err)
	}
	ref := cluster.SessionRef{SessionID: u.SessionID, Generation: u.SessionGeneration}
	if err := srv.registry.Bind(ref, fakeRemoteEdge); err != nil {
		t.Fatal(err)
	}
	if sink != nil {
		if err := srv.control.BeginSync(ref, sink, controlDeferredLimit); err != nil {
			t.Fatal(err)
		}
		if err := srv.control.CommitSync(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
	} else if err := srv.registry.CommitActive(ref); err != nil {
		t.Fatal(err)
	}
	return ref
}

// A remote session (no local conn) can be a VoiceTarget owner, an explicit
// whisper target and a channel shout recipient.
func TestRemoteSessionUsesVoiceTarget(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	a := &mumble.User{Name: "a", ChannelID: root}
	ac, _ := connectTestUser(t, s, a)
	r := &mumble.User{Name: "remote", ChannelID: root, Address: "203.0.113.7"}
	connectRemoteTestUser(t, s, r, nil)

	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{Session: []uint32{r.SessionID}})
	assertVoiceRecipients(t, s, a.SessionID, r.SessionID)

	ch := s.chans.Create(root, "shout", "", 0, false, 0)
	s.users.SetChannel(r.SessionID, ch.ID)
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{ChannelID: ch.ID})
	assertVoiceRecipients(t, s, a.SessionID, r.SessionID)

	// The remote session can also own a target aimed at a local user.
	b := &mumble.User{Name: "b", ChannelID: root}
	connectTestUser(t, s, b)
	payload, err := (&messages.VoiceTarget{ID: 1, Targets: []messages.VoiceTargetTarget{{Session: []uint32{b.SessionID}}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.handleVoiceTarget(protocol.MessageVoiceTarget, payload, fakeRemotePeer{sessionID: r.SessionID, generation: r.SessionGeneration}); err != nil {
		t.Fatal(err)
	}
	assertVoiceRecipients(t, s, r.SessionID, b.SessionID)
}

// A disconnect whose callback is delayed past a session ID reuse must clean up
// only its own generation; the session that reused the ID stays untouched.
func TestStaleGenerationDisconnectSparesReusedSession(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "watched", "", 0, false, 0)
	observer := &mumble.User{Name: "observer", ChannelID: root}
	observerSink := &recordingSink{}
	connectRemoteTestUser(t, s, observer, observerSink)

	v1 := &mumble.User{Name: "v1", ChannelID: root}
	v1c, _ := connectTestUser(t, s, v1)
	v1Ref := userRef(v1)
	listenTo(t, s, v1c, []uint32{ch.ID}, nil)

	if !s.UnregisterConn(v1Ref) {
		t.Fatal("generation 1 的正常清理应报告 announced")
	}
	s.users.RemoveIfGeneration(v1.SessionID, v1.SessionGeneration)

	// Generation 2 reuses the same session ID.
	v2 := &mumble.User{Name: "v2", ChannelID: root}
	v2c, _ := connectTestUser(t, s, v2)
	if v2.SessionID != v1.SessionID {
		t.Fatal("未复用 session ID")
	}
	listenTo(t, s, v2c, []uint32{ch.ID}, nil)
	setVoiceTarget(t, s, v2c, messages.VoiceTargetTarget{HasChannelID: true})

	// Delayed cleanup from generation 1 (a late onClose): must touch nothing.
	if s.UnregisterConn(v1Ref) {
		t.Fatal("迟到的旧 generation 清理不得报告 announced")
	}
	if s.beginCloseAnnounced(v1.SessionID, v1.SessionGeneration) {
		t.Fatal("旧 generation 的关闭不得触发广播")
	}
	s.sendThenClose(v1Ref, protocol.MessageUserRemove, &messages.UserRemove{Session: v1.SessionID})

	if !s.registry.Active(userRef(v2)) {
		t.Fatal("新 generation 不再 Active")
	}
	if _, ok := s.users.Snapshot(v2.SessionID); !ok {
		t.Fatal("新 generation 的用户记录被误删")
	}
	if s.conn(v2.SessionID) == nil {
		t.Fatal("新 generation 的本地连接映射被误删")
	}
	if got := s.listeners.ChannelsFor(v2.SessionID); len(got) == 0 {
		t.Fatal("新 generation 的 listener 被误删")
	}
	if got := s.getVoiceTargetRecipients(v2.SessionID, 1); len(got) == 0 {
		t.Fatal("新 generation 的 voicetarget 被误删")
	}
	for _, m := range observerSink.delivered() {
		if m.Type == protocol.MessageUserRemove {
			t.Fatal("复用后的新会话收到了幽灵 UserRemove")
		}
	}
}

// A protocol ban of a remote session (no local conn) must still record a ban
// entry from the Core-stored address.
func TestProtocolBanOfRemoteSessionUsesStoredAddress(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	grantACL(t, s, root, mumble.PermissionBan)
	admin := &mumble.User{Name: "admin", ChannelID: root}
	ac, _ := connectTestUser(t, s, admin)
	remote := &mumble.User{Name: "remote", ChannelID: root, Address: "203.0.113.7"}
	connectRemoteTestUser(t, s, remote, nil)

	ur := &messages.UserRemove{Session: remote.SessionID, Ban: true, Reason: "cheating"}
	payload, err := ur.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.handleUserRemove(protocol.MessageUserRemove, payload, ac); err != nil {
		t.Fatal(err)
	}

	banned := false
	for _, e := range s.bans.List() {
		if e.Address != nil && net.IP(e.Address).Equal(net.ParseIP("203.0.113.7")) {
			banned = true
		}
	}
	if !banned {
		t.Fatalf("远端会话的协议 ban 未记录存储的 IP，bans=%v", s.bans.List())
	}
	if _, ok := s.users.SnapshotByName("remote"); ok {
		t.Fatal("远端用户未被移除")
	}
}

// The post-move PermissionQuery refresh goes through the control transport,
// so remote sessions receive it too.
func TestSelfMovePermissionQueryGoesThroughControlTransport(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	dest := s.chans.Create(root, "dest", "", 0, false, 0)

	u := &mumble.User{Name: "mover", ChannelID: root}
	c, side := connectTestUser(t, s, u)
	payload := marshalUserState(t, &messages.UserState{Session: u.SessionID, ChannelID: dest.ID, SetFields: messages.UserStateSetChannelID})
	if err := s.handleUserState(protocol.MessageUserState, payload, c); err != nil {
		t.Fatal(err)
	}
	found := false
	for {
		kind, data := readMessage(t, side)
		if kind != protocol.MessagePermissionQuery {
			continue
		}
		var pq messages.PermissionQuery
		if err := pq.Unmarshal(data); err != nil {
			t.Fatal(err)
		}
		if pq.ChannelID != dest.ID {
			t.Fatalf("PermissionQuery channel = %d, want %d", pq.ChannelID, dest.ID)
		}
		found = true
		break
	}
	if !found {
		t.Fatal("移动后未收到 PermissionQuery")
	}

	r := &mumble.User{Name: "rmover", ChannelID: root}
	sink := &recordingSink{}
	connectRemoteTestUser(t, s, r, sink)
	payload = marshalUserState(t, &messages.UserState{Session: r.SessionID, ChannelID: dest.ID, SetFields: messages.UserStateSetChannelID})
	if err := s.handleUserState(protocol.MessageUserState, payload, fakeRemotePeer{sessionID: r.SessionID, generation: r.SessionGeneration}); err != nil {
		t.Fatal(err)
	}
	found = false
	for _, m := range sink.delivered() {
		if m.Type == protocol.MessagePermissionQuery {
			found = true
		}
	}
	if !found {
		t.Fatal("远端会话移动后未收到 PermissionQuery")
	}
}

// The legacy recording announcement filters on Core-owned client version
// metadata, so old remote clients receive it too.
func TestRecordingAnnouncementUsesClientVersionMetadata(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	oldVersion := uint64(1)<<48 | 2<<32 | 2<<16 // 1.2.2
	newVersion := uint64(1)<<48 | 5<<32         // 1.5

	oldLocal := &mumble.User{Name: "old-local", ChannelID: root, ClientVersion: oldVersion}
	_, oldSide := connectTestUser(t, s, oldLocal)
	newLocal := &mumble.User{Name: "new-local", ChannelID: root, ClientVersion: newVersion}
	_, newSide := connectTestUser(t, s, newLocal)
	oldRemote := &mumble.User{Name: "old-remote", ChannelID: root, ClientVersion: oldVersion}
	sink := &recordingSink{}
	connectRemoteTestUser(t, s, oldRemote, sink)

	s.broadcastRecordingAnnouncement("someone", true)

	kind, data := readMessage(t, oldSide)
	if kind != protocol.MessageTextMessage {
		t.Fatalf("旧版本本地会话消息类型 = %d, want TextMessage", kind)
	}
	var tm messages.TextMessage
	if err := tm.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	if tm.Message != "User 'someone' started recording" {
		t.Fatalf("公告内容 = %q", tm.Message)
	}
	expectNoBroadcast(t, newSide)
	found := false
	for _, m := range sink.delivered() {
		if m.Type == protocol.MessageTextMessage {
			found = true
		}
	}
	if !found {
		t.Fatal("远端旧版本会话漏收录音公告")
	}
}

// Real frames in flight while the production disconnect cleanup runs: the
// delivery revalidation must drop stale sessions without racing the cleanup.
func TestVoiceDeliveryRacesWithDisconnectCleanup(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	speaker := &mumble.User{Name: "speaker", ChannelID: root}
	connectTestUser(t, s, speaker)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.router.Route(speaker.SessionID, ma.Frame{Codec: ma.CodecOpus, Target: 0, OpusData: []byte{0x80, 0x01}})
		}
	}()
	for i := 0; i < 50; i++ {
		v := &mumble.User{Name: fmt.Sprintf("victim-%d", i), ChannelID: root}
		vc, _ := connectTestUser(t, s, v)
		listenTo(t, s, vc, []uint32{root}, nil)
		s.users.RemoveIfGeneration(v.SessionID, v.SessionGeneration)
		s.UnregisterConn(userRef(v))
	}
	close(stop)
	wg.Wait()
}
