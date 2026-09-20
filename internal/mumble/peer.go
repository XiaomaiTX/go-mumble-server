package mumble

import (
	"errors"
	"github.com/dchote/go-mumble-server/internal/cluster"

	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

var errNonPeerContext = errors.New("control message arrived on a non-peer context")

// Peer is a TRANSITIONAL adapter (deprecated by design) that decouples control
// handlers from *connection.Conn while the Core/Edge split lands. It carries
// only the logical session metadata and control send operation those handlers
// need.
//
// Forbidden capabilities — do NOT add: CryptState accessors, UDP endpoints or
// addresses, UDP crypto/counters, NAT binding, AudioWireMode, audio codecs,
// net.Conn or any socket accessor. A handler that needs one of those is an
// edge-local handler and must keep taking *connection.Conn at its Local Edge
// entry point, or the capability must become a typed Core/Edge event.
//
// Peer will be deleted once ControlTransport and typed session metadata
// replace it in the Remote Edge phase.
type Peer interface {
	SessionID() uint32
	SessionGeneration() uint64
	WriteMessage(msgType protocol.MessageType, msg messages.Message) error
}

// Compile-time proof that the local connection satisfies the control contract.
var _ Peer = (*connection.Conn)(nil)

// peerAdapt is the single place where the opaque handler context narrows to a
// Peer. Control handlers take the typed interface and must not assert
// *connection.Conn themselves.
func (s *Server) peerAdapt(h func(protocol.MessageType, []byte, Peer) error) protocol.MessageHandler {
	return func(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
		p, ok := ctx.(Peer)
		if !ok {
			return errNonPeerContext
		}
		ref := cluster.SessionRef{SessionID: p.SessionID(), Generation: p.SessionGeneration()}
		if !s.registry.Active(ref) {
			return nil
		}
		return h(msgType, payload, controlPeer{Peer: p, server: s, ref: ref})
	}
}

// sessionActive is the generic control-plane precondition: registry lifecycle
// decides business visibility, never the local transport state.
func (s *Server) sessionActive(sessionID uint32) bool {
	ref, ok := s.registry.Ref(sessionID)
	return ok && s.registry.Active(ref)
}

// controlPeer 的回复始终绑定入口验证过的 generation。
type controlPeer struct {
	Peer
	server *Server
	ref    cluster.SessionRef
}

func (p controlPeer) WriteMessage(kind protocol.MessageType, message messages.Message) error {
	return p.server.control.Send(p.ref, cluster.ControlMessage{Type: kind, Message: message})
}
func (s *Server) peerActive(p Peer) bool {
	return s.registry.Active(cluster.SessionRef{SessionID: p.SessionID(), Generation: p.SessionGeneration()})
}
