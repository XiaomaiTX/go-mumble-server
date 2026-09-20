package edge

import (
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
)

// MessageOwnership classifies a client-facing Mumble control message per
// docs/architecture/core-edge-protocol-ownership.md: who interprets the
// message's semantics — the Edge, the Core, or both through typed halves.
type MessageOwnership int

const (
	// OwnershipEdge means the Edge terminates the message: it concerns the
	// client socket, crypto state or wire encoding (e.g. CryptSetup).
	OwnershipEdge MessageOwnership = iota + 1
	// OwnershipCore means only the Core may interpret the message (user,
	// channel, ACL, ban, text, ... business semantics).
	OwnershipCore
	// OwnershipSplit means both sides own a typed half. The Edge half runs
	// first and collects transport metadata; the Core half is reached through
	// typed events/commands (in-process adapters or the edge-core protocol),
	// never by sharing crypto or socket state.
	OwnershipSplit
)

// OwnershipOf is the single classification table for client ingress. New
// message types must be classified here before they can be routed.
func OwnershipOf(t protocol.MessageType) MessageOwnership {
	switch t {
	case protocol.MessageCryptSetup:
		// Key delivery, nonce update and resync are edge-local crypto state.
		return OwnershipEdge
	case protocol.MessageVersion:
		// Edge: client capabilities / audio wire negotiation.
		// Core: business client-version metadata.
		return OwnershipSplit
	case protocol.MessageAuthenticate:
		// Edge: collect TLS/IP/certificate metadata, FIFO the request.
		// Core: authoritative decision, UserID, SessionRef, default channel.
		return OwnershipSplit
	case protocol.MessagePing:
		// TCP Ping terminates at the Edge (connection.Conn); Core only
		// receives the typed latency metric. It never reaches the router in
		// practice — classified for completeness and auditing.
		return OwnershipSplit
	case protocol.MessageUDPTunnel:
		// Edge: decode to a canonical frame per the client's wire mode.
		// Core: sender eligibility and canonical recipients.
		return OwnershipSplit
	default:
		// UserState, ChannelState/Remove, TextMessage, VoiceTarget, ACL,
		// BanList, PermissionQuery, RequestBlob, UserStats, QueryUsers,
		// UserList, ContextAction*, PluginDataTransmission, ... — every
		// business decision is Core-owned. The Edge only relays them.
		return OwnershipCore
	}
}

// ClientIngressRouter is the client message ownership boundary: every inbound
// client control message passes through here exactly once and is routed to
// the edge or core sink according to OwnershipOf. Ownership must never be
// re-decided with `if mode == ...` checks scattered through handlers.
//
// Split-owned messages are initiated by the edge sink, which then reaches the
// Core half through a typed interface (the Local Edge fuses both halves in
// its in-process handlers; a remote Edge forwards through RemoteCore).
type ClientIngressRouter struct {
	edge protocol.MessageHandler
	core protocol.MessageHandler
}

// NewClientIngressRouter builds the router from its two ownership sinks. A
// nil sink makes that side a no-op, which keeps untrusted input harmless
// while a runtime is still wiring up.
func NewClientIngressRouter(edgeSink, coreSink protocol.MessageHandler) *ClientIngressRouter {
	return &ClientIngressRouter{edge: edgeSink, core: coreSink}
}

// Dispatch routes one inbound client message by ownership.
func (r *ClientIngressRouter) Dispatch(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
	if OwnershipOf(msgType) == OwnershipCore {
		if r.core == nil {
			return nil
		}
		return r.core(msgType, payload, ctx)
	}
	if r.edge == nil {
		return nil
	}
	return r.edge(msgType, payload, ctx)
}

// Table adapts the router to connection.Conn's HandlerTable dispatch. Every
// message type funnels into Dispatch, which applies the ownership decision.
func (r *ClientIngressRouter) Table() protocol.HandlerTable {
	table := protocol.NewHandlerTable()
	for i := 0; i < protocol.MessageCount; i++ {
		table[i] = r.Dispatch
	}
	return table
}
