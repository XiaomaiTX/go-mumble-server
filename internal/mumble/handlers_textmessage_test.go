package mumble

import (
	"net"
	"testing"

	"github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

func TestHandleTextMessage_TreeChecksEveryDescendantPermission(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.RootID()
	allowed := srv.chans.Create(root, "allowed", "", 0, false, 0)
	denied := srv.chans.Create(root, "denied", "", 0, false, 0)
	deniedGrandchild := srv.chans.Create(denied.ID, "denied-grandchild", "", 0, false, 0)
	if allowed == nil || denied == nil || deniedGrandchild == nil {
		t.Fatal("create test channels")
	}
	grantACL(t, srv, root, mumble.PermissionTextMessage)
	denyACL(t, srv, denied.ID, mumble.PermissionTextMessage)

	sender := &mumble.User{Name: "sender", UserID: 20, ChannelID: root}
	senderConn, _ := connectTestUser(t, srv, sender)
	allowedUser := &mumble.User{Name: "allowed-user", UserID: 21, ChannelID: allowed.ID}
	_, allowedClient := connectTestUser(t, srv, allowedUser)
	deniedUser := &mumble.User{Name: "denied-user", UserID: 22, ChannelID: denied.ID}
	_, deniedClient := connectTestUser(t, srv, deniedUser)
	grandchildUser := &mumble.User{Name: "grandchild-user", UserID: 23, ChannelID: deniedGrandchild.ID}
	_, grandchildClient := connectTestUser(t, srv, grandchildUser)

	payload := marshalTextMessage(t, &messages.TextMessage{TreeID: []uint32{root}, Message: "fleet update"})
	if err := srv.handleTextMessage(protocol.MessageTextMessage, payload, senderConn); err != nil {
		t.Fatalf("handleTextMessage: %v", err)
	}

	got := readTextMessage(t, allowedClient)
	if got.Actor != sender.SessionID || got.Message != "fleet update" {
		t.Fatalf("allowed message = %+v", got)
	}
	expectNoPacket(t, deniedClient)
	expectNoPacket(t, grandchildClient)
}

func TestHandleTextMessage_OverlappingTreesDeduplicateRecipients(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.RootID()
	child := srv.chans.Create(root, "child", "", 0, false, 0)
	if child == nil {
		t.Fatal("create child channel")
	}
	grantACL(t, srv, root, mumble.PermissionTextMessage)

	sender := &mumble.User{Name: "sender", UserID: 30, ChannelID: root}
	senderConn, _ := connectTestUser(t, srv, sender)
	recipient := &mumble.User{Name: "recipient", UserID: 31, ChannelID: child.ID}
	_, recipientClient := connectTestUser(t, srv, recipient)

	payload := marshalTextMessage(t, &messages.TextMessage{TreeID: []uint32{root, child.ID}, Message: "once"})
	if err := srv.handleTextMessage(protocol.MessageTextMessage, payload, senderConn); err != nil {
		t.Fatalf("handleTextMessage: %v", err)
	}

	if got := readTextMessage(t, recipientClient); got.Message != "once" {
		t.Fatalf("message = %q, want once", got.Message)
	}
	expectNoPacket(t, recipientClient)
}

func marshalTextMessage(t *testing.T, tm *messages.TextMessage) []byte {
	t.Helper()
	payload, err := tm.Marshal()
	if err != nil {
		t.Fatalf("Marshal TextMessage: %v", err)
	}
	return payload
}

func readTextMessage(t *testing.T, conn net.Conn) *messages.TextMessage {
	t.Helper()
	msgType, payload := readMessage(t, conn)
	if msgType != protocol.MessageTextMessage {
		t.Fatalf("message type = %d, want TextMessage (%d)", msgType, protocol.MessageTextMessage)
	}
	var tm messages.TextMessage
	if err := tm.Unmarshal(payload); err != nil {
		t.Fatalf("Unmarshal TextMessage: %v", err)
	}
	return &tm
}

func expectNoPacket(t *testing.T, conn net.Conn) {
	t.Helper()
	expectNoBroadcast(t, conn)
}
