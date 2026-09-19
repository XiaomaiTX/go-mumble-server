package mumble

import (
	"net"
	"testing"

	"github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

func TestHandleBanList_QueryRequiresBanPermission(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.RootID()
	if err := srv.bans.Replace([]messages.BanEntry{{
		Address: net.IPv4(192, 0, 2, 10).To4(),
		Mask:    32,
		Name:    "private-user",
		Hash:    "private-certificate-hash",
		Reason:  "private-reason",
	}}); err != nil {
		t.Fatalf("seed bans: %v", err)
	}

	requester := &mumble.User{Name: "requester", UserID: 10, ChannelID: root}
	c, clientSide := connectTestUser(t, srv, requester)
	payload := marshalBanList(t, &messages.BanList{Query: true})
	if err := srv.handleBanList(protocol.MessageBanList, payload, c); err != nil {
		t.Fatalf("handleBanList: %v", err)
	}

	denied := readPermissionDenied(t, clientSide)
	if denied.Type != messages.DenyPermission {
		t.Errorf("deny type = %d, want DenyPermission (%d)", denied.Type, messages.DenyPermission)
	}
}

func TestHandleBanList_AuthorizedQueryReturnsList(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.RootID()
	grantACL(t, srv, root, mumble.PermissionBan)
	want := messages.BanEntry{Name: "blocked-user", Hash: "abcdef", Reason: "test ban", Mask: 32}
	if err := srv.bans.Replace([]messages.BanEntry{want}); err != nil {
		t.Fatalf("seed bans: %v", err)
	}

	admin := &mumble.User{Name: "admin", UserID: 11, ChannelID: root}
	c, clientSide := connectTestUser(t, srv, admin)
	payload := marshalBanList(t, &messages.BanList{Query: true})
	if err := srv.handleBanList(protocol.MessageBanList, payload, c); err != nil {
		t.Fatalf("handleBanList: %v", err)
	}

	got := readBanList(t, clientSide)
	if got.Query {
		t.Error("response query = true, want false")
	}
	if len(got.Bans) != 1 || got.Bans[0].Name != want.Name || got.Bans[0].Hash != want.Hash || got.Bans[0].Reason != want.Reason {
		t.Fatalf("response bans = %+v, want seeded ban", got.Bans)
	}
}

func TestHandleBanList_EmptyUpdateClearsAndPersists(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.RootID()
	grantACL(t, srv, root, mumble.PermissionBan)
	if err := srv.bans.Replace([]messages.BanEntry{{Name: "obsolete", Hash: "abcdef", Mask: 32}}); err != nil {
		t.Fatalf("seed bans: %v", err)
	}

	admin := &mumble.User{Name: "admin", UserID: 12, ChannelID: root}
	c, clientSide := connectTestUser(t, srv, admin)
	payload := marshalBanList(t, &messages.BanList{Query: false, Bans: nil})
	if err := srv.handleBanList(protocol.MessageBanList, payload, c); err != nil {
		t.Fatalf("handleBanList: %v", err)
	}

	if got := readBanList(t, clientSide); len(got.Bans) != 0 {
		t.Fatalf("response bans = %+v, want empty", got.Bans)
	}
	if got := srv.bans.List(); len(got) != 0 {
		t.Fatalf("in-memory bans = %+v, want empty", got)
	}
	srv.bans.Reload()
	if got := srv.bans.List(); len(got) != 0 {
		t.Fatalf("reloaded bans = %+v, want empty", got)
	}
}

func TestHandleBanList_UnauthorizedEmptyUpdateDoesNotClear(t *testing.T) {
	srv := newACLTestServer(t)
	root := srv.chans.RootID()
	if err := srv.bans.Replace([]messages.BanEntry{{Name: "keep", Hash: "abcdef", Mask: 32}}); err != nil {
		t.Fatalf("seed bans: %v", err)
	}

	requester := &mumble.User{Name: "requester", UserID: 13, ChannelID: root}
	c, clientSide := connectTestUser(t, srv, requester)
	payload := marshalBanList(t, &messages.BanList{Query: false, Bans: nil})
	if err := srv.handleBanList(protocol.MessageBanList, payload, c); err != nil {
		t.Fatalf("handleBanList: %v", err)
	}

	readPermissionDenied(t, clientSide)
	if got := srv.bans.List(); len(got) != 1 || got[0].Name != "keep" {
		t.Fatalf("bans after denied update = %+v, want original entry", got)
	}
}

func marshalBanList(t *testing.T, bl *messages.BanList) []byte {
	t.Helper()
	payload, err := bl.Marshal()
	if err != nil {
		t.Fatalf("Marshal BanList: %v", err)
	}
	return payload
}

func readBanList(t *testing.T, conn net.Conn) *messages.BanList {
	t.Helper()
	msgType, payload := readMessage(t, conn)
	if msgType != protocol.MessageBanList {
		t.Fatalf("message type = %d, want BanList (%d)", msgType, protocol.MessageBanList)
	}
	var bl messages.BanList
	if err := bl.Unmarshal(payload); err != nil {
		t.Fatalf("Unmarshal BanList: %v", err)
	}
	return &bl
}
