package mumble

import (
	"github.com/dchote/go-mumble-server/internal/acl"
	"github.com/dchote/go-mumble-server/internal/database/models"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
	"testing"
)

func TestPermissionQueryWriteMaskAndSuppression(t *testing.T) {
	s := newACLTestServer(t)
	root := s.chans.RootID()
	child := s.chans.Create(root, "child", "", 0, false, 0)
	grantACL(t, s, child.ID, mumble.PermissionWrite)
	denyACL(t, s, child.ID, mumble.PermissionSpeak|mumble.PermissionWhisper)
	u := &mumble.User{Name: "writer", UserID: 7, ChannelID: child.ID}
	c, peer := connectTestUser(t, s, u)
	payload, err := (&messages.PermissionQuery{ChannelID: child.ID}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.handlePermissionQuery(protocol.MessagePermissionQuery, payload, c); err != nil {
		t.Fatal(err)
	}
	kind, data := readMessage(t, peer)
	if kind != protocol.MessagePermissionQuery {
		t.Fatal("未返回 PermissionQuery")
	}
	var reply messages.PermissionQuery
	if err := reply.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	if reply.Permissions&uint32(mumble.PermissionSpeak|mumble.PermissionWhisper|mumble.PermissionKick|mumble.PermissionBan) != 0 {
		t.Fatalf("查询返回越权掩码 %#x", reply.Permissions)
	}
	if reply.Permissions&uint32(mumble.PermissionListen|mumble.PermissionMove) != uint32(mumble.PermissionListen|mumble.PermissionMove) {
		t.Fatal("Write 展开缺少权限")
	}
	s.RefreshSuppressStates()
	if state := readUserState(t, peer); !state.Suppress {
		t.Fatal("Write 用户被拒 Speak 后未 suppress")
	}
}

func TestUserCannotEnterPastAncestorTraverseDeny(t *testing.T) {
	s := newACLTestServer(t)
	root := s.chans.RootID()
	a := s.chans.Create(root, "a", "", 0, false, 0)
	b := s.chans.Create(a.ID, "b", "", 0, false, 0)
	if err := acl.CreateACL(s.db, &models.ChannelACL{ServerID: s.chans.ServerID(), ChannelID: uint(a.ID), ApplyHere: true, GroupName: "all", Deny: uint32(mumble.PermissionTraverse)}); err != nil {
		t.Fatal(err)
	}
	grantACL(t, s, b.ID, mumble.PermissionEnter)
	u := &mumble.User{Name: "guest", ChannelID: root}
	c, peer := connectTestUser(t, s, u)
	payload := marshalUserState(t, &messages.UserState{ChannelID: b.ID, SetFields: messages.UserStateSetChannelID})
	if err := s.handleUserState(protocol.MessageUserState, payload, c); err != nil {
		t.Fatal(err)
	}
	readPermissionDenied(t, peer)
	if live, _ := s.users.Snapshot(u.SessionID); live.ChannelID != root {
		t.Fatal("越过祖先 Traverse 拒绝进入子频道")
	}
}

func TestWhisperListenerGroupUsesCaseInsensitiveTokens(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "target", "", 0, false, 0)
	a := &mumble.User{Name: "sender", ChannelID: root}
	ac, _ := connectTestUser(t, s, a)
	b := &mumble.User{Name: "listener", ChannelID: root, AccessTokens: []string{"SeCrEt"}}
	bc, _ := connectTestUser(t, s, b)
	listenTo(t, s, bc, []uint32{ch.ID}, nil)
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{ChannelID: ch.ID, Group: "#secret"})
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID)
	s.users.UpdateUser(b.SessionID, func(u *mumble.User) { u.AccessTokens = nil })
	assertVoiceRecipients(t, s, a.SessionID)
}
