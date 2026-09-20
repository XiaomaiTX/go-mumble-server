package edge

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"github.com/dchote/go-mumble-server/pkg/mumble/crypto"
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
type EdgeRuntime struct {
	cfg  *config.Config
	core RemoteCore
	udp  net.PacketConn

	mu            sync.RWMutex
	conns         map[uint32]*connection.Conn
	sessions      map[uint32]coreSession
	bySession     map[cluster.SessionRef]uint32
	udpBySession  map[cluster.SessionRef]net.Addr
	sessionByAddr map[string]cluster.SessionRef
	nextRef       uint32
}

// coreSession is the Core-assigned identity adopted by an edge-local
// connection after a successful Authenticate. The connection keeps its
// ClientConnRef for relay addressing; the Core identity is what the
// initial-sync relay (network phase) will address replies through.
type coreSession struct {
	sessionID  uint32
	generation uint64
	cryptoMode string
}

// NewEdgeRuntime builds the runtime around its Remote Core handle.
func NewEdgeRuntime(cfg *config.Config, core RemoteCore) *EdgeRuntime {
	e := &EdgeRuntime{
		cfg:           cfg,
		core:          core,
		conns:         make(map[uint32]*connection.Conn),
		sessions:      make(map[uint32]coreSession),
		bySession:     make(map[cluster.SessionRef]uint32),
		udpBySession:  make(map[cluster.SessionRef]net.Addr),
		sessionByAddr: make(map[string]cluster.SessionRef),
	}
	if transport, ok := core.(RemoteCoreTransport); ok {
		transport.SetDelivery(e)
	}
	return e
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
	var session cluster.SessionRef
	e.mu.Lock()
	if cur, ok := e.conns[ref]; ok && cur == c {
		if s, exists := e.sessions[ref]; exists {
			session = cluster.SessionRef{SessionID: s.sessionID, Generation: s.generation}
			delete(e.bySession, session)
			if addr := e.udpBySession[session]; addr != nil {
				delete(e.sessionByAddr, addr.String())
			}
			delete(e.udpBySession, session)
		}
		delete(e.conns, ref)
		delete(e.sessions, ref)
	}
	e.mu.Unlock()
	if session.Valid() {
		if transport, ok := e.core.(RemoteCoreTransport); ok {
			transport.Disconnect(session)
		}
	}
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
		return SendReject(c, messages.RejectAuthenticatorFail, "Core unavailable")
	}
	if result.Rejected {
		return SendReject(c, result.RejectType, result.RejectReason)
	}
	// The Core accepted the session. The connection keeps its edge-local
	// ClientConnRef (relay addressing); the Core-assigned identity is recorded
	// separately because the CryptSetup/sync relay and the voice up/down paths
	// arrive with the edge-core network protocol — until then the connection
	// stays parked.
	session := cluster.SessionRef{SessionID: result.SessionID, Generation: result.Generation}
	if !session.Valid() {
		return SendReject(c, messages.RejectAuthenticatorFail, "Invalid Core session")
	}
	mode := negotiateRemoteCrypto(c)
	key, encNonce, decNonce := remoteCryptMaterial(mode)
	c.Crypt = crypto.NewCryptState(mode)
	if err := c.Crypt.SetKey(key, encNonce, decNonce); err != nil {
		return err
	}
	if err := c.WriteMessage(protocol.MessageCryptSetup, &messages.CryptSetup{Key: key, ClientNonce: decNonce, ServerNonce: encNonce}); err != nil {
		return err
	}
	e.mu.Lock()
	cryptoName := "legacy"
	if mode == crypto.ModeLite {
		cryptoName = "lite"
	} else if mode == crypto.ModeSecure {
		cryptoName = "secure"
	}
	e.sessions[ref] = coreSession{sessionID: session.SessionID, generation: session.Generation, cryptoMode: cryptoName}
	e.bySession[session] = ref
	e.mu.Unlock()
	c.SetSessionGeneration(session.Generation)
	name := result.Username
	if name == "" {
		name = authMsg.Username
	}
	c.SetUserName(name)
	transport, ok := e.core.(RemoteCoreTransport)
	if !ok {
		return nil
	}
	if client, ok := transport.(*RemoteCoreClient); ok {
		if err := client.transportReadyWithMode(context.Background(), ref, session, cryptoName); err != nil {
			return SendReject(c, messages.RejectAuthenticatorFail, "Core unavailable")
		}
	} else if err := transport.TransportReady(context.Background(), ref, session); err != nil {
		return SendReject(c, messages.RejectAuthenticatorFail, "Core unavailable")
	}
	slog.Info("edge: transport ready", "session", result.SessionID, "edge_conn", ref, "user", result.Username)
	return nil
}

func negotiateRemoteCrypto(c *connection.Conn) crypto.Mode {
	modes := c.ClientCryptoModes()
	if modes == 0 {
		modes = 0x02
	}
	if modes&0x04 != 0 {
		if tc, ok := c.Conn.(*tls.Conn); ok {
			st := tc.ConnectionState()
			if st.Version == tls.VersionTLS13 && len(st.PeerCertificates) > 0 {
				return crypto.ModeSecure
			}
		}
	}
	if modes&0x02 != 0 {
		return crypto.ModeLegacy
	}
	if modes&0x01 != 0 {
		return crypto.ModeLite
	}
	return crypto.ModeLegacy
}

func remoteCryptMaterial(mode crypto.Mode) (key, encNonce, decNonce []byte) {
	switch mode {
	case crypto.ModeSecure:
		key = make([]byte, 32)
		encNonce = make([]byte, 12)
		decNonce = make([]byte, 12)
	case crypto.ModeLegacy:
		key = make([]byte, 16)
		encNonce = make([]byte, 16)
		decNonce = make([]byte, 16)
	default:
		return []byte{}, nil, nil
	}
	_, _ = rand.Read(key)
	_, _ = rand.Read(encNonce)
	_, _ = rand.Read(decNonce)
	return
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
// decode with the connection's negotiated wire mode.
func (e *EdgeRuntime) handleUDPTunnel(c *connection.Conn, payload []byte) error {
	if c.State() != connection.StateActive {
		return nil
	}
	frame, err := audio.DecodeClientPacket(c.AudioWireMode(), payload)
	if err != nil {
		return err
	}
	connID := c.SessionID()
	e.mu.RLock()
	s, ok := e.sessions[connID]
	e.mu.RUnlock()
	if !ok {
		return nil
	}
	if transport, ok := e.core.(RemoteCoreTransport); ok {
		return transport.VoiceUp(context.Background(), cluster.SessionRef{SessionID: s.sessionID, Generation: s.generation}, frame)
	}
	return ErrCoreProtocolNotImplemented
}

// HandleUDP is the edge-owned UDP ingress. Unencrypted server-list probes are
// answered locally like the Local Edge does.
func (e *EdgeRuntime) HandleUDP(addr net.Addr, data []byte) {
	e.mu.RLock()
	userCount := len(e.sessions)
	e.mu.RUnlock()
	if reply := PlainPingReply(data, userCount, e.cfg.MaxUsers, e.cfg.MaxBandwidth); reply != nil {
		if e.udp != nil {
			_, _ = e.udp.WriteTo(reply, addr)
		}
		return
	}
	plain := make([]byte, len(data)+64)
	var c *connection.Conn
	var ref cluster.SessionRef
	e.mu.RLock()
	if cached, ok := e.sessionByAddr[addr.String()]; ok {
		if connID, found := e.bySession[cached]; found {
			c = e.conns[connID]
			ref = cached
		}
	}
	if c != nil && c.Crypt != nil {
		if err := c.Crypt.Decrypt(plain, data); err == nil {
			plain = plain[:len(data)-c.Crypt.Overhead()]
		} else {
			c = nil
		}
	}
	if c == nil {
		for candidate, connID := range e.bySession {
			conn := e.conns[connID]
			if conn == nil || conn.State() != connection.StateActive || conn.Crypt == nil {
				continue
			}
			if err := conn.Crypt.Decrypt(plain, data); err == nil {
				c = conn
				ref = candidate
				plain = plain[:len(data)-conn.Crypt.Overhead()]
				break
			}
		}
	}
	e.mu.RUnlock()
	if c == nil || len(plain) == 0 {
		return
	}
	e.mu.Lock()
	e.udpBySession[ref] = addr
	e.sessionByAddr[addr.String()] = ref
	e.mu.Unlock()
	mode := c.AudioWireMode()
	isPing := mode == audio.WireLegacy && plain[0]>>5 == 1 || mode == audio.WireProtobuf && plain[0] == 1
	if isPing {
		timestamp, extended, err := audio.DecodePing(mode, plain)
		if err != nil {
			return
		}
		if mode == audio.WireProtobuf {
			if extended {
				plain = ProtobufPingReply(plain[1:], userCount, e.cfg.MaxUsers, e.cfg.MaxBandwidth)
			} else {
				plain = AppendProtoVarintField([]byte{1}, 1, timestamp)
			}
		}
		enc := make([]byte, len(plain)+c.Crypt.Overhead())
		if c.Crypt.Encrypt(enc, plain) == nil && e.udp != nil {
			_, _ = e.udp.WriteTo(enc, addr)
		}
		return
	}
	frame, err := audio.DecodeClientPacket(mode, plain)
	if err != nil {
		return
	}
	if transport, ok := e.core.(RemoteCoreTransport); ok {
		_ = transport.VoiceUp(context.Background(), ref, frame)
	}
}

// DeliverControl is the Core-to-client control sink. The exact SessionRef is
// revalidated immediately before touching the physical connection.
func (e *EdgeRuntime) DeliverControl(msg cluster.ControlWire) {
	ref := msg.Session
	e.mu.RLock()
	connID, ok := e.bySession[ref]
	c := e.conns[connID]
	s := e.sessions[connID]
	e.mu.RUnlock()
	if !ok || c == nil || s.sessionID != ref.SessionID || s.generation != ref.Generation {
		return
	}
	if protocol.MessageType(msg.MessageType) >= protocol.MessageCount {
		return
	}
	if err := c.WriteRaw(protocol.MessageType(msg.MessageType), append([]byte(nil), msg.Payload...)); err != nil {
		return
	}
	if protocol.MessageType(msg.MessageType) == protocol.MessageServerConfig {
		c.SetActive()
	}
}

func (e *EdgeRuntime) FlushControl(ref cluster.SessionRef) error {
	e.mu.RLock()
	id := e.bySession[ref]
	c := e.conns[id]
	e.mu.RUnlock()
	if c == nil {
		return cluster.ErrStaleSession
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return c.AwaitFlush(ctx)
}

func (e *EdgeRuntime) DeliverVoice(batch cluster.VoiceBatch) {
	cache := audio.NewEncodingCache()
	for _, recipient := range batch.Recipients {
		e.mu.RLock()
		connID, ok := e.bySession[recipient.Session]
		c := e.conns[connID]
		addr := e.udpBySession[recipient.Session]
		e.mu.RUnlock()
		if !ok || c == nil || c.State() != connection.StateActive {
			continue
		}
		d := audio.Delivery{Frame: batch.Frame, Context: recipient.Context, VolumeAdjustment: recipient.Volume}
		d.HasPosition = recipient.HasPosition
		packet, err := cache.Encode(c.AudioWireMode(), d)
		if err != nil {
			continue
		}
		if !recipient.ForceTunnel && addr != nil && e.udp != nil && c.Crypt != nil {
			enc := make([]byte, len(packet)+c.Crypt.Overhead())
			if c.Crypt.Encrypt(enc, packet) == nil {
				if _, err = e.udp.WriteTo(enc, addr); err == nil {
					continue
				}
			}
		}
		_ = c.WriteRaw(protocol.MessageUDPTunnel, packet)
	}
}

func (e *EdgeRuntime) CloseSession(ref cluster.SessionRef) {
	e.mu.RLock()
	id := e.bySession[ref]
	c := e.conns[id]
	e.mu.RUnlock()
	if c != nil {
		go c.CloseAfterFlush()
	}
}

// CoreDisconnected implements deterministic fail-closed behavior: no session
// survives a lost authoritative Core connection.
func (e *EdgeRuntime) CoreDisconnected(_ cluster.CoreEpoch) {
	e.mu.RLock()
	list := make([]*connection.Conn, 0, len(e.conns))
	for _, c := range e.conns {
		list = append(list, c)
	}
	e.mu.RUnlock()
	for _, c := range list {
		_ = c.Close()
	}
}

var _ RemoteDelivery = (*EdgeRuntime)(nil)
