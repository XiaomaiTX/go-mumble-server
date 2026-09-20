package mumble

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// Plan 0014 stage 7 control-plane integration tests: they drive the real
// authentication lifecycle path (Bind → BeginSync → sendSync → CommitSync →
// announce) against net.Pipe sockets and assert the wire-visible ordering
// guarantees, rollback, kick/ban final-message delivery and disconnect races.

// waitForCondition polls cond until it holds or the deadline fails the test.
func waitForCondition(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within deadline: %s", desc)
}

// TestAuthenticateSyncOrderAndAnnounce verifies the client-visible initial
// sync order (CryptSetup → CodecVersion → ChannelStates → self UserState →
// Active roster → ServerSync → ServerConfig), that the session only becomes
// Active after the write receipt, and that other clients learn about the join
// only after the commit.
func TestAuthenticateSyncOrderAndAnnounce(t *testing.T) {
	for _, mode := range []config.RuntimeMode{config.ModeStandalone, config.ModeCore} {
		t.Run(string(mode), func(t *testing.T) { testAuthenticateSyncOrderAndAnnounce(t, mode) })
	}
}
func testAuthenticateSyncOrderAndAnnounce(t *testing.T, mode config.RuntimeMode) {
	srv := newACLTestServer(t)
	srv.cfg.Mode = mode
	root := srv.chans.GetTree()[0]
	observer := &mumble.User{Name: "observer", ChannelID: root.ID}
	_, observerSide := connectTestUser(t, srv, observer)

	c, clientSide := newAuthenticatingConn(t)
	authDone := make(chan error, 1)
	go func() {
		payload, err := (&messages.Authenticate{Username: "bob"}).Marshal()
		if err != nil {
			authDone <- err
			return
		}
		authDone <- srv.handleAuthenticate(protocol.MessageAuthenticate, payload, c)
	}()

	var types []protocol.MessageType
	var userStates []*messages.UserState
	var serverSync *messages.ServerSync
	for {
		msgType, payload := readMessage(t, clientSide)
		types = append(types, msgType)
		switch msgType {
		case protocol.MessageUserState:
			var us messages.UserState
			if err := us.Unmarshal(payload); err != nil {
				t.Fatalf("Unmarshal UserState: %v", err)
			}
			userStates = append(userStates, &us)
		case protocol.MessageServerSync:
			var ss messages.ServerSync
			if err := ss.Unmarshal(payload); err != nil {
				t.Fatalf("Unmarshal ServerSync: %v", err)
			}
			serverSync = &ss
		case protocol.MessageServerConfig:
			if serverSync == nil {
				t.Fatal("ServerConfig arrived before ServerSync")
			}
		}
		if msgType == protocol.MessageServerConfig {
			break
		}
	}
	// The session must not be Active until the write receipt is confirmed.
	select {
	case err := <-authDone:
		if err != nil {
			t.Fatalf("handleAuthenticate: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("authentication did not finish after the initial lane was read")
	}

	if len(types) < 2 || types[0] != protocol.MessageCryptSetup || types[1] != protocol.MessageCodecVersion {
		t.Fatalf("initial messages = %v, want CryptSetup then CodecVersion first", types)
	}
	firstUser := -1
	lastChannel := -1
	for i, mt := range types {
		switch mt {
		case protocol.MessageChannelState:
			if firstUser != -1 {
				t.Fatalf("ChannelState at %d after UserState at %d", i, firstUser)
			}
			lastChannel = i
		case protocol.MessageUserState:
			if firstUser == -1 {
				firstUser = i
			}
		}
	}
	if lastChannel == -1 {
		t.Fatal("no ChannelState in initial sync")
	}
	if firstUser == -1 || len(userStates) != 2 {
		t.Fatalf("UserState count = %d, want self + one roster entry", len(userStates))
	}
	bob, ok := srv.users.SnapshotByName("bob")
	if !ok {
		t.Fatal("bob missing from user manager after authentication")
	}
	if serverSync.Session != bob.SessionID {
		t.Fatalf("ServerSync.Session = %d, want bob's %d", serverSync.Session, bob.SessionID)
	}
	if userStates[0].Session != bob.SessionID {
		t.Fatalf("first UserState session = %d, want self (%d)", userStates[0].Session, bob.SessionID)
	}
	if userStates[1].Session != observer.SessionID {
		t.Fatalf("roster UserState session = %d, want observer %d", userStates[1].Session, observer.SessionID)
	}
	ref, ok := srv.registry.Ref(bob.SessionID)
	if !ok || !srv.registry.Active(ref) {
		t.Fatalf("registry did not commit bob to Active (ref=%v ok=%v)", ref, ok)
	}

	// The join announcement reaches other clients only after the commit.
	join := readUserState(t, observerSide)
	if join.Session != bob.SessionID {
		t.Fatalf("observer join UserState session = %d, want %d", join.Session, bob.SessionID)
	}
}

// TestSyncFailureRollsBackSession kills the socket before authentication so
// the CryptSetup write fails; the half-open session must roll back completely
// (user, registry entry, binding) and the same username must be able to
// authenticate afterwards.
func TestSyncFailureRollsBackSession(t *testing.T) {
	srv := newACLTestServer(t)

	c, clientSide := newAuthenticatingConn(t)
	_ = clientSide.Close()
	// Wait for the read loop to notice and run its deferred Close, so the
	// CryptSetup write deterministically fails with ErrClosed.
	waitForCondition(t, "connection reaches closed state", func() bool {
		return c.State() == connection.StateClosed
	})

	payload, err := (&messages.Authenticate{Username: "bob"}).Marshal()
	if err != nil {
		t.Fatalf("Marshal Authenticate: %v", err)
	}
	if err := srv.handleAuthenticate(protocol.MessageAuthenticate, payload, c); err == nil {
		t.Fatal("handleAuthenticate succeeded on a dead socket")
	}
	if srv.users.Count() != 0 {
		t.Fatalf("users after failed sync = %d, want 0", srv.users.Count())
	}
	if len(srv.registry.Sessions()) != 0 {
		t.Fatalf("registry sessions after failed sync = %d, want 0", len(srv.registry.Sessions()))
	}

	// The name and session slot are free again: a retry on a fresh socket
	// completes the full lifecycle.
	c2, client2 := newAuthenticatingConn(t)
	retryDone := make(chan error, 1)
	go func() {
		retryDone <- srv.handleAuthenticate(protocol.MessageAuthenticate, payload, c2)
	}()
	for {
		msgType, _ := readMessage(t, client2)
		if msgType == protocol.MessageServerConfig {
			break
		}
	}
	if err := <-retryDone; err != nil {
		t.Fatalf("retry handleAuthenticate: %v", err)
	}
	if _, ok := srv.users.SnapshotByName("bob"); !ok {
		t.Fatal("retry authentication did not register bob")
	}
	if got := len(srv.registry.Sessions()); got != 1 {
		t.Fatalf("registry sessions after retry = %d, want 1", got)
	}
}

// TestDeferredBroadcastsSpliceAfterInitialSync locks in the barrier
// semantics: broadcasts aimed at a Syncing session are deferred, produce no
// socket traffic during the initial lane, and are spliced after ServerConfig
// when — and only when — the sync commits.
func TestDeferredBroadcastsSpliceAfterInitialSync(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.GetTree()[0]
	u := &mumble.User{Name: "syncer", ChannelID: root.ID}
	c, clientSide, ref := connectSyncingTestUser(t, srv, u)

	// A broadcast while Syncing must not touch the socket.
	deferred := &messages.UserState{Session: 999, SetFields: messages.UserStateSetSession}
	srv.Broadcast(0, protocol.MessageUserState, deferred)
	expectNoBroadcast(t, clientSide)

	if err := srv.sendSync(c, *u, ref); err != nil {
		t.Fatalf("sendSync: %v", err)
	}
	for {
		msgType, _ := readMessage(t, clientSide)
		if msgType == protocol.MessageServerConfig {
			break
		}
	}
	// Still nothing beyond the initial lane before the commit.
	expectNoBroadcast(t, clientSide)
	if err := srv.control.CommitSync(context.Background(), ref); err != nil {
		t.Fatalf("CommitSync: %v", err)
	}
	if !srv.registry.Active(ref) {
		t.Fatal("registry did not commit to Active")
	}

	spliced := readUserState(t, clientSide)
	if spliced.Session != 999 {
		t.Fatalf("first post-sync message session = %d, want deferred 999", spliced.Session)
	}
	// After the commit, ordinary broadcasts flow immediately.
	post := &messages.UserState{Session: 888, SetFields: messages.UserStateSetSession}
	srv.Broadcast(0, protocol.MessageUserState, post)
	live := readUserState(t, clientSide)
	if live.Session != 888 {
		t.Fatalf("post-commit broadcast session = %d, want 888", live.Session)
	}
}

// TestBanFinalMessageIsLastOnVictimWire covers the ban ordering contract: the
// victim receives the UserRemove(Ban=true) as the final message before the
// socket closes, and other clients get the broadcast regardless.
func TestBanFinalMessageIsLastOnVictimWire(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.GetTree()[0]
	grantACL(t, srv, root.ID, mumble.PermissionBan)
	actor := &mumble.User{Name: "admin", ChannelID: root.ID}
	actorConn, _ := connectTestUser(t, srv, actor)
	victim := &mumble.User{Name: "victim", ChannelID: root.ID}
	_, victimSide := connectTestUser(t, srv, victim)
	observer := &mumble.User{Name: "observer", ChannelID: root.ID}
	_, observerSide := connectTestUser(t, srv, observer)

	removeDone := make(chan error, 1)
	go func() {
		ur := &messages.UserRemove{Session: victim.SessionID, Ban: true, Reason: "cheating"}
		payload, err := ur.Marshal()
		if err != nil {
			removeDone <- err
			return
		}
		removeDone <- srv.handleUserRemove(protocol.MessageUserRemove, payload, actorConn)
	}()

	msgType, payload := readMessage(t, observerSide)
	if msgType != protocol.MessageUserRemove {
		t.Fatalf("observer message type = %d, want UserRemove", msgType)
	}
	var obs messages.UserRemove
	if err := obs.Unmarshal(payload); err != nil {
		t.Fatalf("Unmarshal UserRemove: %v", err)
	}
	if obs.Session != victim.SessionID || !obs.Ban || obs.Actor != actor.SessionID {
		t.Fatalf("observer UserRemove = %+v, want ban on %d by %d", obs, victim.SessionID, actor.SessionID)
	}

	msgType, payload = readMessage(t, victimSide)
	if msgType != protocol.MessageUserRemove {
		t.Fatalf("victim final message type = %d, want UserRemove", msgType)
	}
	var fin messages.UserRemove
	if err := fin.Unmarshal(payload); err != nil {
		t.Fatalf("Unmarshal UserRemove: %v", err)
	}
	if fin.Session != victim.SessionID || !fin.Ban || fin.Reason != "cheating" {
		t.Fatalf("victim final UserRemove = %+v", fin)
	}
	if err := <-removeDone; err != nil {
		t.Fatalf("handleUserRemove: %v", err)
	}
	// SendThenClose: the final message is the last thing on the wire before
	// the socket goes away.
	_ = victimSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := protocol.ReadPacket(victimSide); err == nil {
		t.Fatal("victim socket still readable after final UserRemove")
	}
}

// TestKickDuringSyncIsSilent covers the commit-loses interleaving of the
// disconnect race: a session kicked before its initial sync commits was never
// announced, so other clients must observe neither a join nor a removal.
func TestKickDuringSyncIsSilent(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.GetTree()[0]
	observer := &mumble.User{Name: "observer", ChannelID: root.ID}
	_, observerSide := connectTestUser(t, srv, observer)
	victim := &mumble.User{Name: "victim", ChannelID: root.ID}
	_, victimSide, _ := connectSyncingTestUser(t, srv, victim)

	kickDone := make(chan bool, 1)
	go func() { kickDone <- srv.KickSession(victim.SessionID, "bye") }()

	msgType, payload := readMessage(t, victimSide)
	if msgType != protocol.MessageUserRemove {
		t.Fatalf("victim final message type = %d, want UserRemove", msgType)
	}
	var fin messages.UserRemove
	if err := fin.Unmarshal(payload); err != nil {
		t.Fatalf("Unmarshal UserRemove: %v", err)
	}
	if fin.Session != victim.SessionID || fin.Reason != "bye" {
		t.Fatalf("victim final UserRemove = %+v", fin)
	}
	if !<-kickDone {
		t.Fatal("KickSession reported failure")
	}
	expectNoBroadcast(t, observerSide)
	if _, ok := srv.users.SnapshotByName("victim"); ok {
		t.Fatal("victim still present after kick")
	}
	for _, snap := range srv.registry.Sessions() {
		if snap.Ref.SessionID == victim.SessionID {
			t.Fatal("victim still present in registry after kick")
		}
	}
}

// TestActiveCommitThenKickPairsJoinAndRemove covers the commit-wins
// interleaving: after the initial sync commits and the join is announced, a
// kick produces exactly join → UserRemove on other clients' wires.
func TestActiveCommitThenKickPairsJoinAndRemove(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.GetTree()[0]
	observer := &mumble.User{Name: "observer", ChannelID: root.ID}
	_, observerSide := connectTestUser(t, srv, observer)
	victim := &mumble.User{Name: "victim", ChannelID: root.ID}
	c, victimSide, ref := connectSyncingTestUser(t, srv, victim)

	committed := make(chan error, 1)
	go func() {
		err := srv.sendSync(c, *victim, ref)
		if err == nil {
			err = srv.control.CommitSyncAndAnnounce(context.Background(), ref, func() {
				c.SetActive()
				srv.Broadcast(victim.SessionID, protocol.MessageUserState, userToState(*victim))
			})
		}
		committed <- err
	}()
	for {
		msgType, _ := readMessage(t, victimSide)
		if msgType == protocol.MessageServerConfig {
			break
		}
	}
	if err := <-committed; err != nil {
		t.Fatalf("commit path: %v", err)
	}

	join := readUserState(t, observerSide)
	if join.Session != victim.SessionID {
		t.Fatalf("observer join session = %d, want %d", join.Session, victim.SessionID)
	}
	if !srv.KickSession(victim.SessionID, "later") {
		t.Fatal("KickSession reported failure")
	}
	msgType, payload := readMessage(t, observerSide)
	if msgType != protocol.MessageUserRemove {
		t.Fatalf("observer message type = %d, want UserRemove", msgType)
	}
	var remove messages.UserRemove
	if err := remove.Unmarshal(payload); err != nil {
		t.Fatalf("Unmarshal UserRemove: %v", err)
	}
	if remove.Session != victim.SessionID {
		t.Fatalf("observer remove session = %d, want %d", remove.Session, victim.SessionID)
	}
}

// TestDisconnectRaceWithActiveCommit races the commit path against the
// production disconnect cleanup (server.go onClose shape) under the race
// detector. The invariant is that every join announcement is paired with at
// most one removal and no session ends up with an unpaired message — i.e. no
// ghost joins and no ghost removals.
func TestDisconnectRaceWithActiveCommit(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.GetTree()[0]
	observer := &mumble.User{Name: "observer", ChannelID: root.ID}
	_, observerSide := connectTestUser(t, srv, observer)

	const rounds = 10
	sessions := make(map[uint32]int)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		victim := &mumble.User{Name: fmt.Sprintf("race-%d", i), ChannelID: root.ID}
		c, victimSide, ref := connectSyncingTestUser(t, srv, victim)
		mu.Lock()
		sessions[victim.SessionID] = 0
		mu.Unlock()
		wg.Add(3)
		go func(side net.Conn) {
			defer wg.Done()
			for {
				_ = side.SetReadDeadline(time.Now().Add(2 * time.Second))
				if _, _, err := protocol.ReadPacket(side); err != nil {
					return
				}
			}
		}(victimSide)
		go func(v mumble.User, r cluster.SessionRef) {
			defer wg.Done()
			err := srv.sendSync(c, v, r)
			if err == nil {
				err = srv.control.CommitSyncAndAnnounce(context.Background(), r, func() { c.SetActive(); srv.Broadcast(v.SessionID, protocol.MessageUserState, userToState(v)) })
			}
		}(*victim, ref)
		go func(sid uint32, gen uint64) {
			defer wg.Done()
			// Production cleanup shape from server.go's onClose: the conn pins
			// the exact logical session, so cleanup is generation-aware.
			ref := cluster.SessionRef{SessionID: sid, Generation: gen}
			announced := srv.UnregisterConn(ref)
			srv.users.RemoveIfGeneration(sid, gen)
			if announced {
				srv.Broadcast(sid, protocol.MessageUserRemove, &messages.UserRemove{Session: sid})
			}
		}(victim.SessionID, victim.SessionGeneration)
	}
	wg.Wait()

	joins := make(map[uint32]int)
	removes := make(map[uint32]int)
	for {
		_ = observerSide.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		msgType, payload, err := protocol.ReadPacket(observerSide)
		if err != nil {
			break
		}
		switch msgType {
		case protocol.MessageUserState:
			var us messages.UserState
			if err := us.Unmarshal(payload); err != nil {
				t.Fatalf("Unmarshal UserState: %v", err)
			}
			joins[us.Session]++
		case protocol.MessageUserRemove:
			var ur messages.UserRemove
			if err := ur.Unmarshal(payload); err != nil {
				t.Fatalf("Unmarshal UserRemove: %v", err)
			}
			removes[ur.Session]++
		}
	}
	for sid := range sessions {
		if joins[sid] > 1 {
			t.Fatalf("session %d announced %d times", sid, joins[sid])
		}
		if joins[sid] != removes[sid] {
			t.Fatalf("session %d has %d joins but %d removes (ghost broadcast)", sid, joins[sid], removes[sid])
		}
	}
	for sid := range joins {
		if _, known := sessions[sid]; !known {
			t.Fatalf("unexpected join for unknown session %d", sid)
		}
	}
}

// TestAuthenticateStoresClientVersionMetadata pins the Core-owned copy of the
// split-owned client version: authentication must persist the negotiated
// version onto the user record so Core decisions (legacy recording
// announcement, future capability gating) never read the edge conn.
func TestAuthenticateStoresClientVersionMetadata(t *testing.T) {
	srv := newACLTestServer(t)
	c, clientSide := newAuthenticatingConn(t)

	versionPacked := uint64(1)<<48 | 2<<32 | 2<<16 // 1.2.2
	vp, err := (&messages.Version{VersionV2: versionPacked}).Marshal()
	if err != nil {
		t.Fatalf("Marshal Version: %v", err)
	}
	if err := srv.handleVersion(protocol.MessageVersion, vp, c); err != nil {
		t.Fatalf("handleVersion: %v", err)
	}

	payload, err := (&messages.Authenticate{Username: "bob"}).Marshal()
	if err != nil {
		t.Fatalf("Marshal Authenticate: %v", err)
	}
	authDone := make(chan error, 1)
	go func() { authDone <- srv.handleAuthenticate(protocol.MessageAuthenticate, payload, c) }()
	for {
		msgType, _ := readMessage(t, clientSide)
		if msgType == protocol.MessageServerConfig {
			break
		}
	}
	if err := <-authDone; err != nil {
		t.Fatalf("handleAuthenticate: %v", err)
	}
	bob, ok := srv.users.SnapshotByName("bob")
	if !ok {
		t.Fatal("bob missing after authentication")
	}
	if bob.ClientVersion != versionPacked {
		t.Fatalf("stored ClientVersion = %#x, want %#x", bob.ClientVersion, versionPacked)
	}
}

// TestRejectIsFinalThenClose enforces murmur's reject contract: the Reject is
// the last message on the wire and the connection closes right after, so
// official clients raise the password prompt from the disconnect event
// instead of at a random later moment.
func TestRejectIsFinalThenClose(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.GetTree()[0]
	addTestUser(t, srv, &mumble.User{Name: "bob", ChannelID: root.ID})

	c, clientSide := newAuthenticatingConn(t)
	// Probe the writer like the other fixtures: handleAuthenticate is driven
	// from this goroutine, so Run's write loop may not be scheduled yet.
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
		t.Fatal("socket still readable after Reject; server must disconnect")
	}
}
