package edge

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"

	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// errNonConnContext guards the ingress sinks against foreign contexts.
var errNonConnContext = errors.New("client message arrived on a non-connection context")

// EdgeRuntime is the edge-mode counterpart of the Core's Local Edge: it owns
// the edge-local session state — client connections, their edge-local
// ClientConnRefs, UDP endpoints and CryptState — and forwards every
// Core-owned decision to the Remote Core. It must never gain authoritative
// business state: no user manager, channel manager, ACL evaluator, ban
// manager, identity authority or Core voice router belongs here.
//
// Phase 2A ships the skeleton: the composition, ownership boundary and
// lifecycle are real; the edge-core network protocol is not, so RemoteCore
// fails closed and authentication ends in a Reject.
type EdgeRuntime struct {
	cfg  *config.Config
	core RemoteCore
	udp  net.PacketConn

	mu       sync.RWMutex
	conns    map[uint32]*connection.Conn
	sessions map[uint32]coreSession
	nextRef  uint32
}

// coreSession is the Core-assigned identity adopted by an edge-local
// connection after a successful Authenticate. The connection keeps its
// ClientConnRef for relay addressing; the Core identity is what the
// initial-sync relay (network phase) will address replies through.
type coreSession struct {
	sessionID  uint32
	generation uint64
}

// NewEdgeRuntime builds the runtime around its Remote Core handle.
func NewEdgeRuntime(cfg *config.Config, core RemoteCore) *EdgeRuntime {
	return &EdgeRuntime{
		cfg:      cfg,
		core:     core,
		conns:    make(map[uint32]*connection.Conn),
		sessions: make(map[uint32]coreSession),
	}
}

// BindUDP hands the shared client runtime's UDP socket to the edge so
// edge-owned UDP replies (probe echoes, later voice) can be written.
func (e *EdgeRuntime) BindUDP(conn net.PacketConn) {
	e.udp = conn
}

// SetupConn installs edge-local per-connection wiring. TCP Ping already
// terminates inside connection.Conn; the Core metric sink is not applicable
// to a remote edge (metrics travel with the edge-core protocol instead).
func (e *EdgeRuntime) SetupConn(*connection.Conn) {}

// OnClose is the physical disconnect cleanup: it releases the edge-local
// ClientConnRef. Core-side session teardown is driven by the edge-core
// disconnect event in the network phase; nothing authoritative lives here.
func (e *EdgeRuntime) OnClose(c *connection.Conn) {
	ref := c.SessionID()
	if ref == 0 {
		return
	}
	e.mu.Lock()
	if cur, ok := e.conns[ref]; ok && cur == c {
		delete(e.conns, ref)
		delete(e.sessions, ref)
	}
	e.mu.Unlock()
	slog.Info("Mumble client disconnected", "remote", c.RemoteAddr(), "edge_conn", ref)
}

// coreSessionOf reports the Core-assigned identity adopted by an edge-local
// ClientConnRef, if Authenticate succeeded for it.
func (e *EdgeRuntime) coreSessionOf(ref uint32) (coreSession, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, ok := e.sessions[ref]
	return s, ok
}

// HandleEdgeMessage is the ingress router's edge sink: edge-owned and
// split-owned messages terminate or originate here.
func (e *EdgeRuntime) HandleEdgeMessage(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
	c, ok := ctx.(*connection.Conn)
	if !ok {
		return errNonConnContext
	}
	switch msgType {
	case protocol.MessageVersion:
		return e.handleVersion(c, payload)
	case protocol.MessageAuthenticate:
		return e.handleAuthenticate(c, payload)
	case protocol.MessageCryptSetup:
		return CryptSetupResync(c, payload)
	case protocol.MessageUDPTunnel:
		return e.handleUDPTunnel(c, payload)
	}
	return nil
}

// HandleCoreMessage is the ingress router's core sink: core-owned messages
// are relayed to the Remote Core tagged with the edge-local connection ref.
func (e *EdgeRuntime) HandleCoreMessage(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
	c, ok := ctx.(*connection.Conn)
	if !ok {
		return errNonConnContext
	}
	ref := c.SessionID()
	if ref == 0 {
		// Core-owned traffic from a connection that never authenticated: the
		// Core would reject it, so drop it here.
		return nil
	}
	e.mu.RLock()
	cur, attached := e.conns[ref]
	e.mu.RUnlock()
	if !attached || cur != c {
		// The ref belongs to a different (or released) connection: a stale or
		// forged association must not reach the Core.
		return nil
	}
	return e.core.ForwardControl(context.Background(), ref, msgType, payload)
}

// handleVersion is the edge half of the split-owned Version exchange: audio
// wire-mode negotiation is edge-owned transport state. The Core half (business
// metadata) rides along with the Authenticate forward.
func (e *EdgeRuntime) handleVersion(c *connection.Conn, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	var v messages.Version
	if err := v.Unmarshal(payload); err != nil {
		return err
	}
	c.SetClientCryptoModes(v.CryptoModes)
	c.SetClientVersion(ClientVersionFull(v))
	c.NegotiateAudioWireMode(ProtocolVersionV2)
	slog.Debug("edge: client version", "release", v.Release, "os", v.OS, "wire_mode", c.AudioWireMode())
	return nil
}

// handleAuthenticate is the edge half of the split-owned Authenticate
// exchange: collect TLS/IP/certificate metadata and ask the Core for the
// authoritative decision. Until the edge-core protocol exists the forward
// fails closed with the standard authenticator rejection — the edge never
// substitutes a local Core.
func (e *EdgeRuntime) handleAuthenticate(c *connection.Conn, payload []byte) error {
	var authMsg messages.Authenticate
	if err := authMsg.Unmarshal(payload); err != nil {
		return err
	}
	if authMsg.Username == "" {
		return SendReject(c, messages.RejectInvalidUsername, "Username required")
	}
	remoteIP := ""
	if addr := c.RemoteAddr(); addr != nil {
		if host, _, err := net.SplitHostPort(addr.String()); err == nil && host != "" {
			remoteIP = host
		} else {
			remoteIP = addr.String()
		}
	}
	ref := e.attach(c)
	result, err := e.core.Authenticate(context.Background(), AuthForward{
		ConnID:              ref,
		Username:            authMsg.Username,
		Password:            authMsg.Password,
		Tokens:              authMsg.Tokens,
		CertHash:            c.CertificateHash(),
		CertificateVerified: c.CertificateVerified(),
		RemoteIP:            remoteIP,
		ClientVersion:       c.ClientVersion(),
		ClientCryptoModes:   c.ClientCryptoModes(),
	})
	if err != nil {
		if !errors.Is(err, ErrCoreProtocolNotImplemented) {
			slog.Warn("edge: remote core authentication failed", "err", err, "identity_username", authMsg.Username)
		}
		return SendReject(c, messages.RejectAuthenticatorFail, "Edge-Core protocol not implemented")
	}
	if result.Rejected {
		return SendReject(c, result.RejectType, result.RejectReason)
	}
	// The Core accepted the session. The connection keeps its edge-local
	// ClientConnRef (relay addressing); the Core-assigned identity is recorded
	// separately because the CryptSetup/sync relay and the voice up/down paths
	// arrive with the edge-core network protocol — until then the connection
	// stays parked.
	e.mu.Lock()
	e.sessions[ref] = coreSession{sessionID: result.SessionID, generation: result.Generation}
	e.mu.Unlock()
	c.SetUserName(authMsg.Username)
	slog.Info("edge: remote core accepted session; initial sync relay pending protocol", "session", result.SessionID, "edge_conn", ref, "user", authMsg.Username)
	return nil
}

// attach assigns the connection its edge-local ClientConnRef, idempotently
// for a retried Authenticate on the same connection.
func (e *EdgeRuntime) attach(c *connection.Conn) uint32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ref := c.SessionID(); ref != 0 {
		if cur, ok := e.conns[ref]; ok && cur == c {
			return ref
		}
	}
	e.nextRef++
	ref := e.nextRef
	e.conns[ref] = c
	c.SetSessionID(ref)
	return ref
}

// handleUDPTunnel is the edge half of the split-owned UDPTunnel exchange:
// decode with the connection's negotiated wire mode. Forwarding the canonical
// frame upstream (VoiceUp) is edge-core protocol work and is deferred; the
// decoded frame is validated and dropped so malformed input still fails
// visibly.
func (e *EdgeRuntime) handleUDPTunnel(c *connection.Conn, payload []byte) error {
	if c.State() != connection.StateActive {
		return nil
	}
	if _, err := audio.DecodeClientPacket(c.AudioWireMode(), payload); err != nil {
		return err
	}
	return nil
}

// HandleUDP is the edge-owned UDP ingress. Unencrypted server-list probes are
// answered locally like the Local Edge does. Encrypted voice/ping requires
// authenticated sessions, which require the edge-core auth protocol, so those
// datagrams are dropped in this phase.
func (e *EdgeRuntime) HandleUDP(addr net.Addr, data []byte) {
	if reply := PlainPingReply(data, 0, e.cfg.MaxUsers, e.cfg.MaxBandwidth); reply != nil {
		if e.udp != nil {
			_, _ = e.udp.WriteTo(reply, addr)
		}
		return
	}
}
