package mumble

import (
	"log/slog"
	"net"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/internal/edge"
	pkgmumble "github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// LocalEdge adapts the shared client runtime's physical transport events onto
// the frozen Core contracts. standalone and core compose the exact same
// LocalEdge; it owns no business state of its own:
//
//	control ingress  → cluster.ControlTransport via controlSink (FIFO, barrier)
//	voice egress     → cluster.Dispatcher via localVoiceTransport (VoiceBatch)
//	lifecycle        → cluster.Registry (Syncing → Active → Closing → Closed)
//	edge protocol    → the Server's edge-local handler entries
//
// It must not re-implement ACL, UserManager, ChannelManager, VoiceTarget,
// listener or recipient routing — every decision stays in the Core.
type LocalEdge struct {
	core *Server
}

// NewLocalEdge builds the adapter for an in-process Core.
func NewLocalEdge(core *Server) *LocalEdge {
	return &LocalEdge{core: core}
}

// Ingress returns the client ingress router with the Local Edge and Core
// halves of this process wired to their ownership sinks.
func (e *LocalEdge) Ingress() *edge.ClientIngressRouter {
	return edge.NewClientIngressRouter(e.core.EdgeIngressHandler(), e.core.CoreIngressHandler())
}

// SetupConn installs the Core-side sink for client-advertised TCP latency.
// Ping replies themselves terminate inside connection.Conn at the edge.
func (e *LocalEdge) SetupConn(c *connection.Conn) {
	c.SetPingReporter(func(pc *connection.Conn, avgMicros float32) {
		e.core.UserManager().SetPing(pc.SessionID(), avgMicros)
	})
}

// HandleUDP feeds one UDP datagram to the edge-owned UDP ingress.
func (e *LocalEdge) HandleUDP(addr net.Addr, data []byte) {
	e.core.HandleUDP(addr, data)
}

// remoteAddrString recovers the peer address for disconnect logs; net.Conn
// implementations cache it, so it stays readable after close.
func remoteAddrString(c *connection.Conn) string {
	if addr := c.RemoteAddr(); addr != nil {
		return addr.String()
	}
	return ""
}

// OnClose is the physical disconnect cleanup: it drives the generation-aware
// Core teardown for the logical session the connection was authenticated as.
// The conn pins the exact logical session (ID + generation), so a disconnect
// callback delayed past a session ID reuse cleans up only its own generation.
func (e *LocalEdge) OnClose(c *connection.Conn) {
	ms := e.core
	sid := c.SessionID()
	name := c.UserName()
	ref := cluster.SessionRef{SessionID: sid, Generation: c.SessionGeneration()}
	var removed bool
	var u pkgmumble.User
	if ref.Valid() {
		announced := ms.UnregisterConn(ref)
		u, removed = ms.UserManager().RemoveIfGeneration(sid, ref.Generation)
		if removed && u.ChannelID != 0 {
			ms.UpdateChannelCrypto(u.ChannelID)
		}
		// Only sessions whose join was announced get a UserRemove; a
		// never-announced session must not produce a ghost removal.
		if announced {
			ms.Broadcast(sid, protocol.MessageUserRemove, &messages.UserRemove{Session: sid, Actor: 0})
		}
	}
	if name != "" || removed {
		n := name
		if removed {
			n = u.Name
		}
		slog.Info("Mumble client disconnected", "remote", remoteAddrString(c), "session", sid, "user", n)
		return
	}
	slog.Info("Mumble client disconnected", "remote", remoteAddrString(c), "session", sid)
}
