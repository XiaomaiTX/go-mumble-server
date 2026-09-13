package mumble

import (
	"bytes"
	"net"
	"testing"

	"github.com/dchote/go-mumble-server/internal/channel"
	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// listenTo drives a listening-only UserState (no has-bits set) through the real
// handler. SetFields stays 0 on purpose: this is exactly the shape a Mumble 1.4+
// client sends when it only toggles channel listening, and the handler must not
// drop it at the SetFields==0 gate.
func listenTo(t *testing.T, s *Server, c *connection.Conn, add, remove []uint32, vols ...messages.VolumeAdjustment) {
	t.Helper()
	payload := marshalUserState(t, &messages.UserState{
		ListeningChannelAdd:       add,
		ListeningChannelRemove:    remove,
		ListeningVolumeAdjustment: vols,
	})
	if err := s.handleUserState(protocol.MessageUserState, payload, c); err != nil {
		t.Fatalf("handleUserState: %v", err)
	}
}

func equalChannels(got, want []uint32) bool {
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

// drainBroadcast consumes the one UserState each connected side receives for a
// single applied listening change (the fan-out hits every client, not just the
// listener).
func drainBroadcast(t *testing.T, sides ...net.Conn) {
	t.Helper()
	for _, side := range sides {
		readUserState(t, side)
	}
}

// A listening-only UserState must apply and broadcast — the regression this
// guards against is the message dying at the SetFields==0 early return before
// listening fields were pulled aside.
func TestUserStateListeningAddBroadcastsAppliedDelta(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "listened", "", 0, false, 0)
	a := &mumble.User{Name: "a", ChannelID: root}
	ac, aSide := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b", ChannelID: root}
	_, bSide := connectTestUser(t, s, b)

	listenTo(t, s, ac, []uint32{ch.ID}, nil)

	if got := s.listeners.ChannelsFor(a.SessionID); !equalChannels(got, []uint32{ch.ID}) {
		t.Fatalf("ChannelsFor = %v, want [%d]", got, ch.ID)
	}
	for _, side := range []struct {
		name string
		conn net.Conn
	}{{"sender", aSide}, {"other", bSide}} {
		us := readUserState(t, side.conn)
		if us.Session != a.SessionID || us.Actor != a.SessionID {
			t.Fatalf("%s: session=%d actor=%d, want %d", side.name, us.Session, us.Actor, a.SessionID)
		}
		if !equalChannels(us.ListeningChannelAdd, []uint32{ch.ID}) {
			t.Fatalf("%s: ListeningChannelAdd = %v, want [%d]", side.name, us.ListeningChannelAdd, ch.ID)
		}
	}
}

func TestUserStateListeningAddUnknownChannelSkipped(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	a := &mumble.User{Name: "a"}
	ac, aSide := connectTestUser(t, s, a)

	listenTo(t, s, ac, []uint32{9999}, nil)

	if got := s.listeners.ChannelsFor(a.SessionID); len(got) != 0 {
		t.Fatalf("ChannelsFor = %v, want empty", got)
	}
	expectNoBroadcast(t, aSide)
}

func TestUserStateListeningDenyListenPermissionPartialSuccess(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	chAllowed := s.chans.Create(root, "allowed", "", 0, false, 0)
	chDenied := s.chans.Create(root, "denied", "", 0, false, 0)
	denyACL(t, s, chDenied.ID, mumble.PermissionListen)
	a := &mumble.User{Name: "a", ChannelID: root}
	ac, aSide := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b", ChannelID: root}
	_, bSide := connectTestUser(t, s, b)

	listenTo(t, s, ac, []uint32{chAllowed.ID, chDenied.ID}, nil)

	// Denials go to the sender only and precede the broadcast of applied deltas.
	denied := readPermissionDenied(t, aSide)
	if denied.Type != messages.DenyPermission {
		t.Fatalf("deny type = %d, want DenyPermission", denied.Type)
	}
	if denied.Permission != uint32(mumble.PermissionListen) {
		t.Fatalf("deny permission = %#x, want Listen (%#x)", denied.Permission, mumble.PermissionListen)
	}
	if denied.ChannelID != chDenied.ID {
		t.Fatalf("deny channel = %d, want %d", denied.ChannelID, chDenied.ID)
	}
	us := readUserState(t, aSide)
	if !equalChannels(us.ListeningChannelAdd, []uint32{chAllowed.ID}) {
		t.Fatalf("sender broadcast add = %v, want only [%d]", us.ListeningChannelAdd, chAllowed.ID)
	}
	other := readUserState(t, bSide)
	if !equalChannels(other.ListeningChannelAdd, []uint32{chAllowed.ID}) {
		t.Fatalf("other broadcast add = %v, want only [%d]", other.ListeningChannelAdd, chAllowed.ID)
	}
	if got := s.listeners.ChannelsFor(a.SessionID); !equalChannels(got, []uint32{chAllowed.ID}) {
		t.Fatalf("ChannelsFor = %v, want [%d]", got, chAllowed.ID)
	}
}

func TestUserStateListeningTargetingOtherSessionIgnored(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "listened", "", 0, false, 0)
	a := &mumble.User{Name: "a", ChannelID: root}
	ac, aSide := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b", ChannelID: root}
	_, bSide := connectTestUser(t, s, b)

	payload := marshalUserState(t, &messages.UserState{
		Session:             b.SessionID,
		SetFields:           messages.UserStateSetSession,
		ListeningChannelAdd: []uint32{ch.ID},
	})
	if err := s.handleUserState(protocol.MessageUserState, payload, ac); err != nil {
		t.Fatalf("handleUserState: %v", err)
	}

	if got := s.listeners.ChannelsFor(b.SessionID); len(got) != 0 {
		t.Fatalf("victim ChannelsFor = %v, want empty", got)
	}
	expectNoBroadcast(t, aSide)
	expectNoBroadcast(t, bSide)
}

func TestUserStateListeningRemoveBroadcasts(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "listened", "", 0, false, 0)
	a := &mumble.User{Name: "a", ChannelID: root}
	ac, aSide := connectTestUser(t, s, a)

	listenTo(t, s, ac, []uint32{ch.ID}, nil)
	readUserState(t, aSide)

	listenTo(t, s, ac, nil, []uint32{ch.ID})

	us := readUserState(t, aSide)
	if !equalChannels(us.ListeningChannelRemove, []uint32{ch.ID}) {
		t.Fatalf("ListeningChannelRemove = %v, want [%d]", us.ListeningChannelRemove, ch.ID)
	}
	if got := s.listeners.ChannelsFor(a.SessionID); len(got) != 0 {
		t.Fatalf("ChannelsFor = %v, want empty", got)
	}
}

// Volume adjustments are stored and broadcast to the owner only, matching
// murmur's broadcastListenerVolumeAdjustments=false default.
func TestUserStateListeningVolumeStoredAndOwnerOnly(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "listened", "", 0, false, 0)
	a := &mumble.User{Name: "a", ChannelID: root}
	ac, aSide := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b", ChannelID: root}
	_, bSide := connectTestUser(t, s, b)

	listenTo(t, s, ac, []uint32{ch.ID}, nil)
	readUserState(t, aSide)
	readUserState(t, bSide)

	listenTo(t, s, ac, nil, nil, messages.VolumeAdjustment{ListeningChannel: ch.ID, VolumeAdjustment: 2.5})

	own := readUserState(t, aSide)
	if len(own.ListeningVolumeAdjustment) != 1 ||
		own.ListeningVolumeAdjustment[0].ListeningChannel != ch.ID ||
		own.ListeningVolumeAdjustment[0].VolumeAdjustment != 2.5 {
		t.Fatalf("owner volumes = %+v, want [{%d 2.5}]", own.ListeningVolumeAdjustment, ch.ID)
	}
	other := readUserState(t, bSide)
	if len(other.ListeningVolumeAdjustment) != 0 {
		t.Fatalf("other client saw volumes %+v, want none", other.ListeningVolumeAdjustment)
	}
	vols := s.listeners.ListeningVolumes(a.SessionID)
	if len(vols) != 1 || vols[0].VolumeAdjustment != 2.5 {
		t.Fatalf("stored volumes = %+v, want 2.5", vols)
	}
}

// Volume for a channel the session does not listen to is ignored, mirroring
// murmur's warning-and-skip.
func TestUserStateListeningVolumeWithoutListenIgnored(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "listened", "", 0, false, 0)
	a := &mumble.User{Name: "a", ChannelID: root}
	ac, aSide := connectTestUser(t, s, a)

	listenTo(t, s, ac, nil, nil, messages.VolumeAdjustment{ListeningChannel: ch.ID, VolumeAdjustment: 2.5})

	if vols := s.listeners.ListeningVolumes(a.SessionID); len(vols) != 0 {
		t.Fatalf("stored volumes = %+v, want none", vols)
	}
	expectNoBroadcast(t, aSide)
}

// Target 0 speech must reach listeners of the speaker's channel, skip deafened
// listeners, and skip users who are merely in another channel.
func TestVoiceRoutesToListenersTargetZero(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch1 := s.chans.Create(root, "one", "", 0, false, 0)
	ch2 := s.chans.Create(root, "two", "", 0, false, 0)
	a := &mumble.User{Name: "a", ChannelID: ch1.ID}
	_, aSide := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b", ChannelID: ch2.ID}
	bc, bSide := connectTestUser(t, s, b)
	c := &mumble.User{Name: "c", ChannelID: ch2.ID}
	_, cSide := connectTestUser(t, s, c)
	deaf := &mumble.User{Name: "deaf", ChannelID: ch2.ID}
	dcc, dSide := connectTestUser(t, s, deaf)
	s.users.UpdateUser(deaf.SessionID, func(u *mumble.User) { u.SelfDeaf = true })

	listenTo(t, s, bc, []uint32{ch1.ID}, nil)
	drainBroadcast(t, aSide, bSide, cSide, dSide)
	listenTo(t, s, dcc, []uint32{ch1.ID}, nil)
	drainBroadcast(t, aSide, bSide, cSide, dSide)

	pkt := []byte{0x80, 0x01, 0xAA, 0xBB}
	if err := s.router.Route(a.SessionID, 0, pkt); err != nil {
		t.Fatalf("Route: %v", err)
	}

	kind, got := readMessage(t, bSide)
	if kind != protocol.MessageUDPTunnel || !bytes.Equal(got, pkt) {
		t.Fatalf("listener got type=%d pkt=%v, want UDPTunnel %v", kind, got, pkt)
	}
	expectNoBroadcast(t, cSide)
	expectNoBroadcast(t, dSide)
}

// Speech in a channel reaches listeners of linked channels too.
func TestVoiceRoutesToLinkedChannelListeners(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch1 := s.chans.Create(root, "one", "", 0, false, 0)
	ch2 := s.chans.Create(root, "two", "", 0, false, 0)
	ch3 := s.chans.Create(root, "three", "", 0, false, 0)
	s.chans.Update(ch1.ID, channel.UpdateOpts{Links: []uint32{ch2.ID}})
	a := &mumble.User{Name: "a", ChannelID: ch1.ID}
	connectTestUser(t, s, a)
	b := &mumble.User{Name: "b", ChannelID: ch3.ID}
	bc, bSide := connectTestUser(t, s, b)

	listenTo(t, s, bc, []uint32{ch2.ID}, nil)
	readUserState(t, bSide)

	pkt := []byte{0x80, 0x01, 0xCC}
	if err := s.router.Route(a.SessionID, 0, pkt); err != nil {
		t.Fatalf("Route: %v", err)
	}
	kind, got := readMessage(t, bSide)
	if kind != protocol.MessageUDPTunnel || !bytes.Equal(got, pkt) {
		t.Fatalf("linked listener got type=%d pkt=%v, want UDPTunnel %v", kind, got, pkt)
	}
}

// A denied linked channel must not receive normal speech, while other channels
// in the same complete component remain reachable.
func TestVoiceRoutesLinkedChannelsCheckSpeakPerTarget(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	aChannel := s.chans.Create(root, "a", "", 0, false, 0)
	bChannel := s.chans.Create(root, "b-denied", "", 0, false, 0)
	cChannel := s.chans.Create(root, "c-allowed", "", 0, false, 0)
	if aChannel == nil || bChannel == nil || cChannel == nil {
		t.Fatal("create channels")
	}
	if _, ok := s.chans.UpdateLinks(aChannel.ID, []uint32{bChannel.ID}); !ok {
		t.Fatal("link a-b")
	}
	if _, ok := s.chans.UpdateLinks(bChannel.ID, []uint32{aChannel.ID, cChannel.ID}); !ok {
		t.Fatal("link b-c")
	}
	denyACL(t, s, bChannel.ID, mumble.PermissionSpeak)

	sender := &mumble.User{Name: "sender", ChannelID: aChannel.ID}
	connectTestUser(t, s, sender)
	denied := &mumble.User{Name: "denied", ChannelID: bChannel.ID}
	_, deniedSide := connectTestUser(t, s, denied)
	allowed := &mumble.User{Name: "allowed", ChannelID: cChannel.ID}
	_, allowedSide := connectTestUser(t, s, allowed)

	pkt := []byte{0x80, 0x01, 0xDD}
	if err := s.router.Route(sender.SessionID, 0, pkt); err != nil {
		t.Fatal(err)
	}
	expectNoBroadcast(t, deniedSide)
	kind, got := readMessage(t, allowedSide)
	if kind != protocol.MessageUDPTunnel || !bytes.Equal(got, pkt) {
		t.Fatalf("allowed target got type=%d packet=%v, want UDPTunnel %v", kind, got, pkt)
	}
}

// Whisper/shout channel targets include listeners, and the group restriction
// filters them exactly like occupants.
func TestWhisperChannelTargetIncludesListeners(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "target", "", 0, false, 0)
	elsewhere := s.chans.Create(root, "elsewhere", "", 0, false, 0)
	a := &mumble.User{Name: "a", ChannelID: root}
	ac, _ := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b", ChannelID: elsewhere.ID}
	bc, _ := connectTestUser(t, s, b)

	listenTo(t, s, bc, []uint32{ch.ID}, nil)

	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{ChannelID: ch.ID})
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID)

	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{ChannelID: ch.ID, Group: "#secret"})
	assertVoiceRecipients(t, s, a.SessionID)
	s.users.UpdateUser(b.SessionID, func(u *mumble.User) { u.AccessTokens = []string{"secret"} })
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID)
}

func TestUnregisterConnDropsListeners(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "listened", "", 0, false, 0)
	a := &mumble.User{Name: "a", ChannelID: ch.ID}
	connectTestUser(t, s, a)
	b := &mumble.User{Name: "b", ChannelID: root}
	bc, bSide := connectTestUser(t, s, b)

	listenTo(t, s, bc, []uint32{ch.ID}, nil)
	readUserState(t, bSide)

	s.UnregisterConn(b.SessionID)

	if got := s.listeners.ChannelsFor(b.SessionID); len(got) != 0 {
		t.Fatalf("ChannelsFor after unregister = %v, want empty", got)
	}
	if got := s.listeners.SessionIDsIn(ch.ID); len(got) != 0 {
		t.Fatalf("SessionIDsIn after unregister = %v, want empty", got)
	}
	if err := s.router.Route(a.SessionID, 0, []byte{0x80, 0x01}); err != nil {
		t.Fatalf("Route: %v", err)
	}
	expectNoBroadcast(t, bSide)
}

// Deleting a channel must strip listeners and tell them via a
// listening_channel_remove UserState that arrives before the ChannelRemove.
func TestChannelRemoveDropsListenersWithUserStateBeforeChannelRemove(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "listened", "", 0, false, 0)
	grantACL(t, s, ch.ID, mumble.PermissionWrite)
	a := &mumble.User{Name: "a", ChannelID: root}
	ac, _ := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b", ChannelID: root}
	bc, bSide := connectTestUser(t, s, b)

	listenTo(t, s, bc, []uint32{ch.ID}, nil)
	readUserState(t, bSide)

	payload, err := (&messages.ChannelRemove{ChannelID: ch.ID}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.handleChannelRemove(protocol.MessageChannelRemove, payload, ac); err != nil {
		t.Fatal(err)
	}

	us := readUserState(t, bSide)
	if us.Session != b.SessionID || !equalChannels(us.ListeningChannelRemove, []uint32{ch.ID}) {
		t.Fatalf("listening_remove UserState = {session %d, remove %v}, want {%d, [%d]}",
			us.Session, us.ListeningChannelRemove, b.SessionID, ch.ID)
	}
	kind, _ := readMessage(t, bSide)
	if kind != protocol.MessageChannelRemove {
		t.Fatalf("next message type = %d, want ChannelRemove (%d)", kind, protocol.MessageChannelRemove)
	}
	if got := s.listeners.SessionIDsIn(ch.ID); len(got) != 0 {
		t.Fatalf("SessionIDsIn after channel remove = %v, want empty", got)
	}
}

// A client syncing in must learn every user's listening list; its own snapshot
// carries its volumes.
func TestSendSyncIncludesListeningLists(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "listened", "", 0, false, 0)
	b := &mumble.User{Name: "b", ChannelID: root}
	bc, _ := connectTestUser(t, s, b)
	listenTo(t, s, bc, []uint32{ch.ID}, nil,
		messages.VolumeAdjustment{ListeningChannel: ch.ID, VolumeAdjustment: 1.5})

	c := &mumble.User{Name: "c", ChannelID: root}
	cc, cSide := connectTestUser(t, s, c)
	s.sendSync(cc, *c)

	foundB := false
	for i := 0; i < 32 && !foundB; i++ {
		kind, payload := readMessage(t, cSide)
		if kind != protocol.MessageUserState {
			continue
		}
		var us messages.UserState
		if err := us.Unmarshal(payload); err != nil {
			t.Fatalf("Unmarshal UserState: %v", err)
		}
		if us.Session == b.SessionID {
			foundB = true
			if !equalChannels(us.ListeningChannelAdd, []uint32{ch.ID}) {
				t.Fatalf("sync listening list = %v, want [%d]", us.ListeningChannelAdd, ch.ID)
			}
			if len(us.ListeningVolumeAdjustment) != 0 {
				t.Fatalf("sync leaked other user's volumes: %+v", us.ListeningVolumeAdjustment)
			}
		}
	}
	if !foundB {
		t.Fatal("sync never delivered b's UserState")
	}
}

func TestListenerLimitDenials(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "listened", "", 0, false, 0)
	ch2 := s.chans.Create(root, "other", "", 0, false, 0)
	a := &mumble.User{Name: "a", ChannelID: root}
	ac, aSide := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b", ChannelID: root}
	bc, bSide := connectTestUser(t, s, b)

	s.cfg.MaxChannelListeners = 1
	listenTo(t, s, ac, []uint32{ch.ID}, nil)
	drainBroadcast(t, aSide, bSide)
	listenTo(t, s, bc, []uint32{ch.ID}, nil)
	denied := readPermissionDenied(t, bSide)
	if denied.Type != messages.DenyChannelListenerLimit {
		t.Fatalf("deny type = %d, want DenyChannelListenerLimit (%d)", denied.Type, messages.DenyChannelListenerLimit)
	}
	expectNoBroadcast(t, bSide)

	// Per-user cap: a already listens to ch; a second channel is refused.
	s.cfg.MaxChannelListeners = 0
	s.cfg.MaxListenersPerUser = 1
	listenTo(t, s, ac, []uint32{ch2.ID}, nil)
	denied = readPermissionDenied(t, aSide)
	if denied.Type != messages.DenyUserListenerLimit {
		t.Fatalf("deny type = %d, want DenyUserListenerLimit (%d)", denied.Type, messages.DenyUserListenerLimit)
	}
	expectNoBroadcast(t, aSide)
	if got := s.listeners.ChannelsFor(a.SessionID); !equalChannels(got, []uint32{ch.ID}) {
		t.Fatalf("ChannelsFor = %v, want only [%d]", got, ch.ID)
	}
}
