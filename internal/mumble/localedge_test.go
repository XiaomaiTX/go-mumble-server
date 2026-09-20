package mumble

import (
	"testing"
	"time"

	"github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// LocalEdge.OnClose must reproduce the acceptLoop disconnect cleanup exactly:
// an announced Active session is removed generation-safely and its UserRemove
// is broadcast; the shared client runtime delegates here on every physical
// disconnect.
func TestLocalEdgeOnCloseCleansUpAnnouncedSession(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.GetTree()[0]
	observer := &mumble.User{Name: "observer", ChannelID: root.ID}
	_, observerSide := connectTestUser(t, srv, observer)
	victim := &mumble.User{Name: "victim", ChannelID: root.ID}
	victimConn, _ := connectTestUser(t, srv, victim)

	NewLocalEdge(srv).OnClose(victimConn)

	msgType, payload := readMessage(t, observerSide)
	if msgType != protocol.MessageUserRemove {
		t.Fatalf("observer message type = %d, want UserRemove", msgType)
	}
	var remove messages.UserRemove
	if err := remove.Unmarshal(payload); err != nil {
		t.Fatalf("Unmarshal UserRemove: %v", err)
	}
	if remove.Session != victim.SessionID {
		t.Fatalf("remove session = %d, want %d", remove.Session, victim.SessionID)
	}
	if _, ok := srv.users.SnapshotByName("victim"); ok {
		t.Fatal("victim still present after LocalEdge.OnClose")
	}
	if _, ok := srv.registry.Ref(victim.SessionID); ok {
		t.Fatal("victim still bound in registry after LocalEdge.OnClose")
	}
	if srv.conn(victim.SessionID) != nil {
		t.Fatal("victim conn still registered after LocalEdge.OnClose")
	}
}

// A session that never completed its initial sync was never announced, so its
// disconnect must produce no ghost UserRemove.
func TestLocalEdgeOnCloseOfSyncingSessionIsSilent(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.GetTree()[0]
	observer := &mumble.User{Name: "observer", ChannelID: root.ID}
	_, observerSide := connectTestUser(t, srv, observer)
	victim := &mumble.User{Name: "victim", ChannelID: root.ID}
	victimConn, _, _ := connectSyncingTestUser(t, srv, victim)

	NewLocalEdge(srv).OnClose(victimConn)

	expectNoBroadcast(t, observerSide)
	if _, ok := srv.users.SnapshotByName("victim"); ok {
		t.Fatal("syncing victim still present after LocalEdge.OnClose")
	}
}

// LocalEdge feeds the shared client runtime: the ingress router carries both
// ownership sinks and SetupConn installs the Core metric sink without
// touching business state.
func TestLocalEdgeWiresClientRuntimeHooks(t *testing.T) {
	srv := newACLTestServer(t)
	le := NewLocalEdge(srv)

	router := le.Ingress()
	if router == nil {
		t.Fatal("LocalEdge produced no ingress router")
	}
	table := router.Table()
	if table[protocol.MessageCryptSetup] == nil || table[protocol.MessageUserState] == nil {
		t.Fatal("ingress table has holes")
	}
	c, _ := newAuthenticatingConn(t)
	le.SetupConn(c)
}

// The ownership split of the fused in-process handlers: Edge-owned CryptSetup
// dispatches through the edge half (transport state), core-owned UserState
// through the core half (typed Peer), one classification, no per-handler mode
// checks.
func TestLocalEdgeIngressRoutesByOwnership(t *testing.T) {
	srv := newACLTestServer(t)
	router := NewLocalEdge(srv).Ingress()
	c, _ := newAuthenticatingConn(t)

	// Edge-owned: CryptSetup on a conn without Crypt state is a no-op.
	if err := router.Dispatch(protocol.MessageCryptSetup, nil, c); err != nil {
		t.Fatalf("Dispatch(CryptSetup): %v", err)
	}
	// Split-owned: Version with no payload is accepted by the edge half.
	if err := router.Dispatch(protocol.MessageVersion, nil, c); err != nil {
		t.Fatalf("Dispatch(Version): %v", err)
	}
	// Core-owned messages without a peer context fail closed.
	if err := router.Dispatch(protocol.MessageUserState, nil, nil); err == nil {
		t.Fatal("Dispatch(UserState) with a nil context must fail closed")
	}
}

// The reject path is shared with remote edges through edge.SendReject: the
// Reject is the final message and the socket closes right after.
func TestServerRejectUsesSharedEdgeFlushThenClose(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.GetTree()[0]
	addTestUser(t, srv, &mumble.User{Name: "bob", ChannelID: root.ID})

	c, clientSide := newAuthenticatingConn(t)
	if err := c.WriteMessage(protocol.MessagePing, &messages.Ping{}); err != nil {
		t.Fatalf("queue writer readiness probe: %v", err)
	}
	if _, _, err := protocol.ReadPacket(clientSide); err != nil {
		t.Fatalf("read writer readiness probe: %v", err)
	}
	payload, err := (&messages.Authenticate{Username: "bob"}).Marshal()
	if err != nil {
		t.Fatalf("Marshal Authenticate: %v", err)
	}
	if err := srv.handleAuthenticate(protocol.MessageAuthenticate, payload, c); err != nil {
		t.Fatalf("handleAuthenticate: %v", err)
	}
	msgType, out := readMessage(t, clientSide)
	if msgType != protocol.MessageReject {
		t.Fatalf("message type = %d, want Reject", msgType)
	}
	var reject messages.Reject
	if err := reject.Unmarshal(out); err != nil {
		t.Fatalf("Unmarshal Reject: %v", err)
	}
	if reject.Type != messages.RejectUsernameInUse {
		t.Fatalf("reject type = %d, want RejectUsernameInUse", reject.Type)
	}
	_ = clientSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := protocol.ReadPacket(clientSide); err == nil {
		t.Fatal("socket still readable after Reject; edge.SendReject must close after flush")
	}
}
