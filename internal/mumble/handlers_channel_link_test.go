package mumble

import (
	"testing"

	"github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

func TestHandleChannelStateLinkRequiresBothEndpoints(t *testing.T) {
	s := newACLTestServer(t)
	root := s.chans.RootID()
	a := s.chans.Create(root, "a", "", 0, false, 0)
	b := s.chans.Create(root, "b", "", 0, false, 0)
	if a == nil || b == nil {
		t.Fatal("create channels")
	}
	actor := &mumble.User{Name: "actor", UserID: 7, ChannelID: root}
	actorConn, actorSide := connectTestUser(t, s, actor)
	grantACL(t, s, a.ID, mumble.PermissionLinkChannel)

	payload, err := (&messages.ChannelState{ChannelID: a.ID, LinksAdd: []uint32{b.ID}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.handleChannelState(protocol.MessageChannelState, payload, actorConn); err != nil {
		t.Fatal(err)
	}
	readPermissionDenied(t, actorSide)
	gotA, _ := s.chans.GetChannel(a.ID)
	gotB, _ := s.chans.GetChannel(b.ID)
	if len(gotA.Links) != 0 || len(gotB.Links) != 0 {
		t.Fatalf("unauthorized link changed channels: a=%v b=%v", gotA.Links, gotB.Links)
	}

	grantACL(t, s, b.ID, mumble.PermissionLinkChannel)
	if err := s.handleChannelState(protocol.MessageChannelState, payload, actorConn); err != nil {
		t.Fatal(err)
	}
	gotA, _ = s.chans.GetChannel(a.ID)
	gotB, _ = s.chans.GetChannel(b.ID)
	if len(gotA.Links) != 1 || gotA.Links[0] != b.ID ||
		len(gotB.Links) != 1 || gotB.Links[0] != a.ID {
		t.Fatalf("authorized link is not bidirectional: a=%v b=%v", gotA.Links, gotB.Links)
	}
}
