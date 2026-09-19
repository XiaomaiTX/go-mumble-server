package mumble

import (
	"net"
	"testing"

	"github.com/dchote/go-mumble-server/internal/acl"
	"github.com/dchote/go-mumble-server/internal/database/models"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/wire"
)

// Root is the one channel with no parent, and the proto2 parent field must be
// absent rather than 0 — 0 is root's own ID. Mumla's protocol library looks the
// parent up the moment a ChannelState carries one and dereferences the result
// without a null check (humla ModelHandler.messageChannelState), so a root that
// claims a parent kills the client mid-sync, which is what issue #1's follow-up
// reported as "Mumla abruptly disconnects".
func TestChannelToState_RootOmitsParent(t *testing.T) {
	state := channelToState(&mumble.Channel{ID: 0, Name: "Root"})

	if state.HasParent {
		t.Errorf("root announced parent %d; the field must be absent for root", state.Parent)
	}
	if state.ChannelID != 0 {
		t.Errorf("root channel ID = %d, want 0", state.ChannelID)
	}

	payload, err := state.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if fieldPresentOnWire(t, payload, 2) {
		t.Error("marshaled root ChannelState still carries field 2 (parent); Mumla would NPE")
	}
}

func TestChannelToState_ChildCarriesItsParent(t *testing.T) {
	state := channelToState(&mumble.Channel{ID: 4, ParentID: 0, Name: "lobby"})

	if !state.HasParent {
		t.Fatal("child omitted its parent, so clients cannot place it in the tree")
	}
	if state.Parent != 0 {
		t.Errorf("parent = %d, want root 0", state.Parent)
	}

	payload, err := state.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !fieldPresentOnWire(t, payload, 2) {
		t.Error("marshaled child ChannelState omitted field 2 (parent)")
	}
}

func TestChannelState_MarshalsExplicitFalseEnterFlags(t *testing.T) {
	state := &messages.ChannelState{
		ChannelID:          3,
		HasEnterRestricted: true,
		HasCanEnter:        true,
	}

	payload, err := state.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !fieldPresentOnWire(t, payload, 12) {
		t.Fatal("is_enter_restricted=false 未写入线路，客户端会保留旧锁状态")
	}
	if !fieldPresentOnWire(t, payload, 13) {
		t.Fatal("can_enter=false 未写入线路，客户端无法显示红锁")
	}

	var decoded messages.ChannelState
	if err := decoded.Unmarshal(payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !decoded.HasEnterRestricted || decoded.IsEnterRestricted {
		t.Fatalf("is_enter_restricted 解码结果错误: present=%v value=%v", decoded.HasEnterRestricted, decoded.IsEnterRestricted)
	}
	if !decoded.HasCanEnter || decoded.CanEnter {
		t.Fatalf("can_enter 解码结果错误: present=%v value=%v", decoded.HasCanEnter, decoded.CanEnter)
	}
}

func TestRefreshEnterStates_PersonalizesLockStateAndClearsIt(t *testing.T) {
	srv := newACLTestServer(t)
	child := srv.chans.Create(srv.chans.RootID(), "受限频道", "", 0, false, 0)
	denyACL(t, srv, child.ID, mumble.PermissionEnter)

	allowedID := uint32(7)
	allowedACLID := int32(allowedID)
	if err := acl.CreateACL(srv.db, &models.ChannelACL{
		ServerID:  srv.chans.ServerID(),
		ChannelID: uint(child.ID),
		Priority:  2,
		ApplyHere: true,
		UserID:    &allowedACLID,
		Grant:     uint32(mumble.PermissionEnter),
	}); err != nil {
		t.Fatalf("创建用户 Enter 授权: %v", err)
	}
	srv.acl.InvalidateCache()

	allowed := &mumble.User{Name: "allowed", UserID: allowedID, ChannelID: srv.chans.RootID()}
	_, allowedPeer := connectTestUser(t, srv, allowed)
	denied := &mumble.User{Name: "denied", UserID: 8, ChannelID: srv.chans.RootID()}
	_, deniedPeer := connectTestUser(t, srv, denied)

	srv.RefreshEnterStates()
	allowedState := readChannelStateFor(t, allowedPeer, child.ID)
	deniedState := readChannelStateFor(t, deniedPeer, child.ID)
	assertEnterState(t, allowedState, true, true)
	assertEnterState(t, deniedState, true, false)

	if err := srv.db.Where("server_id = ? AND channel_id = ?", srv.chans.ServerID(), child.ID).
		Delete(&models.ChannelACL{}).Error; err != nil {
		t.Fatalf("删除频道 ACL: %v", err)
	}
	srv.acl.InvalidateCache()
	srv.RefreshEnterStates()
	assertEnterState(t, readChannelStateFor(t, allowedPeer, child.ID), false, true)
	assertEnterState(t, readChannelStateFor(t, deniedPeer, child.ID), false, true)
}

func readChannelStateFor(t *testing.T, conn net.Conn, channelID uint32) *messages.ChannelState {
	t.Helper()
	for i := 0; i < 10; i++ {
		kind, payload := readMessage(t, conn)
		if kind != protocol.MessageChannelState {
			continue
		}
		var state messages.ChannelState
		if err := state.Unmarshal(payload); err != nil {
			t.Fatalf("Unmarshal ChannelState: %v", err)
		}
		if state.ChannelID == channelID {
			return &state
		}
	}
	t.Fatalf("未收到频道 %d 的 ChannelState", channelID)
	return nil
}

func assertEnterState(t *testing.T, state *messages.ChannelState, restricted, canEnter bool) {
	t.Helper()
	if !state.HasEnterRestricted || state.IsEnterRestricted != restricted {
		t.Errorf("is_enter_restricted: present=%v value=%v, want present=true value=%v", state.HasEnterRestricted, state.IsEnterRestricted, restricted)
	}
	if !state.HasCanEnter || state.CanEnter != canEnter {
		t.Errorf("can_enter: present=%v value=%v, want present=true value=%v", state.HasCanEnter, state.CanEnter, canEnter)
	}
}

// fieldPresentOnWire reports whether a protobuf field number appears in payload.
func fieldPresentOnWire(t *testing.T, payload []byte, fieldNum int) bool {
	t.Helper()
	b := payload
	for len(b) > 0 {
		fn, wt, n, err := wire.ReadTag(b)
		if err != nil {
			t.Fatalf("ReadTag: %v (payload=%x)", err, payload)
		}
		b = b[n:]
		if fn == fieldNum {
			return true
		}
		skip, err := wire.SkipField(b, wt)
		if err != nil {
			t.Fatalf("SkipField: %v", err)
		}
		b = b[skip:]
	}
	return false
}
