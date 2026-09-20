package edge

import (
	"testing"

	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
)

// The ownership classification is exhaustive: every message type maps to
// exactly one of Edge/Split/Core, and no future type can slip in undecided
// (the default is Core, the fail-safe side).
func TestOwnershipOfClassifiesEveryMessageType(t *testing.T) {
	edgeOwned := map[protocol.MessageType]bool{
		protocol.MessageCryptSetup: true,
	}
	split := map[protocol.MessageType]bool{
		protocol.MessageVersion:      true,
		protocol.MessageAuthenticate: true,
		protocol.MessagePing:         true,
		protocol.MessageUDPTunnel:    true,
	}
	for i := 0; i < protocol.MessageCount; i++ {
		mt := protocol.MessageType(i)
		got := OwnershipOf(mt)
		want := OwnershipCore
		if edgeOwned[mt] {
			want = OwnershipEdge
		} else if split[mt] {
			want = OwnershipSplit
		}
		if got != want {
			t.Errorf("OwnershipOf(%d) = %d, want %d", mt, got, want)
		}
	}
}

// The router must send Edge and Split ingress to the edge sink and Core
// ingress to the core sink — the single ownership decision point.
func TestClientIngressRouterRoutesByOwnership(t *testing.T) {
	var edgeSinks, coreSinks []protocol.MessageType
	router := NewClientIngressRouter(
		func(mt protocol.MessageType, _ []byte, _ interface{}) error {
			edgeSinks = append(edgeSinks, mt)
			return nil
		},
		func(mt protocol.MessageType, _ []byte, _ interface{}) error {
			coreSinks = append(coreSinks, mt)
			return nil
		},
	)
	cases := []struct {
		mt   protocol.MessageType
		side string
	}{
		{protocol.MessageCryptSetup, "edge"},
		{protocol.MessageVersion, "edge"},
		{protocol.MessageAuthenticate, "edge"},
		{protocol.MessageUDPTunnel, "edge"},
		{protocol.MessageUserState, "core"},
		{protocol.MessageChannelState, "core"},
		{protocol.MessageTextMessage, "core"},
		{protocol.MessageVoiceTarget, "core"},
		{protocol.MessageACL, "core"},
		{protocol.MessageBanList, "core"},
		{protocol.MessagePermissionQuery, "core"},
		{protocol.MessagePluginDataTransmission, "core"},
	}
	for _, tc := range cases {
		if err := router.Dispatch(tc.mt, nil, nil); err != nil {
			t.Fatalf("Dispatch(%d): %v", tc.mt, err)
		}
	}
	if len(edgeSinks) != 4 {
		t.Errorf("edge sink saw %v, want exactly the 4 edge/split types", edgeSinks)
	}
	if len(coreSinks) != len(cases)-4 {
		t.Errorf("core sink saw %v, want %d core types", coreSinks, len(cases)-4)
	}
}

// A nil sink makes that side a no-op, so untrusted input stays harmless while
// a runtime is still wiring up.
func TestClientIngressRouterNilSinksAreNoOps(t *testing.T) {
	router := NewClientIngressRouter(nil, nil)
	for i := 0; i < protocol.MessageCount; i++ {
		if err := router.Dispatch(protocol.MessageType(i), nil, nil); err != nil {
			t.Fatalf("Dispatch(%d) with nil sinks: %v", i, err)
		}
	}
}

// The HandlerTable adaptation covers every message type through Dispatch.
func TestClientIngressRouterTableRoutesEveryType(t *testing.T) {
	total := 0
	router := NewClientIngressRouter(
		func(protocol.MessageType, []byte, interface{}) error { total++; return nil },
		func(protocol.MessageType, []byte, interface{}) error { total++; return nil },
	)
	table := router.Table()
	for i := 0; i < protocol.MessageCount; i++ {
		if table[i] == nil {
			t.Fatalf("table[%d] is nil; every type must route through Dispatch", i)
		}
		if err := table[i](protocol.MessageType(i), nil, nil); err != nil {
			t.Fatalf("table[%d]: %v", i, err)
		}
	}
	if total != protocol.MessageCount {
		t.Fatalf("dispatched %d messages, want %d", total, protocol.MessageCount)
	}
}
