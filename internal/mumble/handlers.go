package mumble

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dchote/go-mumble-server/internal/acl"
	"github.com/dchote/go-mumble-server/internal/audio"
	"github.com/dchote/go-mumble-server/internal/auth"
	"github.com/dchote/go-mumble-server/internal/ban"
	"github.com/dchote/go-mumble-server/internal/channel"
	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/internal/edge"
	"github.com/dchote/go-mumble-server/internal/identity"
	"github.com/dchote/go-mumble-server/internal/user"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	mumbleaudio "github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"github.com/dchote/go-mumble-server/pkg/mumble/crypto"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
	"gorm.io/gorm"
)

// Server holds Mumble server state and builds the handler table.
type Server struct {
	cfg             *config.Config
	db              *gorm.DB
	users           *user.Manager
	chans           *channel.Manager
	bans            *ban.Manager
	acl             *acl.Evaluator
	table           protocol.HandlerTable
	edgeIngress     protocol.HandlerTable
	coreIngress     protocol.HandlerTable
	connMu          sync.RWMutex
	conns           map[uint32]*connection.Conn
	addrBySession   sync.Map // SessionRef -> net.Addr
	sessionByAddr   sync.Map // addr.String() -> SessionRef
	voiceTargets    sync.Map // session -> map[targetID]voiceTargetSpec；由 connMu 保护更新
	textRateLimiter sync.Map // session -> *rateWindow
	// userStateRateLimiter throttles self-targeted UserState, which murmur also
	// rate-limits because each accepted message fans out to every client.
	userStateRateLimiter sync.Map // session -> *rateWindow
	// listeners tracks Mumble 1.4+ channel-listening state; zero value is usable.
	listeners       listenerManager
	channelCryptoMu sync.RWMutex
	channelCrypto   map[uint32]string // channelID -> "legacy"|"lite"|"secure"|"mixed"|""
	router          *audio.Router
	registry        *cluster.Registry
	control         *cluster.ControlTransport
	dispatcher      *cluster.Dispatcher
	udpConn         net.PacketConn
	voiceDebug      atomic.Bool
	// Content policy, held separately from cfg so REST edits take effect without a
	// restart and without racing the handler goroutines that read them.
	allowRecording atomic.Bool
	maxTextBytes   atomic.Int64
	maxImageBytes  atomic.Int64
	authority      identity.Authority
	authorityErr   error
}

// rateWindow is a fixed one-second window counter guarding a per-session message
// budget.
type rateWindow struct {
	mu       sync.Mutex
	count    int
	windowAt time.Time
}

func (r *rateWindow) allow(limit int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if now.Sub(r.windowAt) > time.Second {
		r.windowAt = now
		r.count = 0
	}
	r.count++
	return r.count <= limit
}

func rateLimitAllows(limiter *sync.Map, sessionID uint32, perSecond int) bool {
	v, _ := limiter.LoadOrStore(sessionID, &rateWindow{})
	return v.(*rateWindow).allow(perSecond)
}

// HandleUDP processes an incoming UDP voice or ping packet.
func (s *Server) HandleUDP(addr net.Addr, data []byte) {
	if s.udpConn == nil {
		return
	}
	// Unencrypted server-list probes are answered before any crypt handling, like
	// Murmur's Server::udpActivated. Senders here have no session, so falling
	// through to the trial-decrypt path would only log noise and drop the probe.
	if reply := edge.PlainPingReply(data, s.users.Count(), s.cfg.MaxUsers, s.cfg.MaxBandwidth); reply != nil {
		s.udpConn.WriteTo(reply, addr)
		return
	}
	voiceDebug := s.voiceDebug.Load()
	plain := make([]byte, len(data)+256)
	var senderSession uint32
	var senderCrypt *crypto.CryptState
	var fromCache bool

	addrKey := addr.String()

	// Primary path: look up session by cached address (set after first successful identification).
	if cached, ok := s.sessionByAddr.Load(addrKey); ok {
		cachedRef := cached.(cluster.SessionRef)
		sid := cachedRef.SessionID
		s.connMu.RLock()
		c, cOk := s.conns[sid]
		s.connMu.RUnlock()
		if cOk && c.SessionGeneration() == cachedRef.Generation && c.Crypt != nil && c.State() == connection.StateActive {
			if err := c.Crypt.Decrypt(plain, data); err == nil {
				senderSession = sid
				senderCrypt = c.Crypt
				plain = plain[:len(data)-c.Crypt.Overhead()]
				fromCache = true
			} else {
				// Cached address no longer valid (NAT rebinding); clear and fall through.
				if voiceDebug {
					slog.Info("[VOICE-DEBUG] HandleUDP: cache decrypt failed (NAT rebinding?), clearing",
						"addr", addrKey, "cached_session", sid)
				}
				s.sessionByAddr.Delete(addrKey)
			}
		}
	}

	// Fallback: trial decrypt for unmapped addresses (first packet from a new client).
	if senderSession == 0 {
		s.connMu.RLock()
		var tryCount, connCount int
		var lastErr error
		connCount = len(s.conns)
		for _, mode := range []crypto.Mode{crypto.ModeSecure, crypto.ModeLegacy, crypto.ModeLite} {
			for sid, c := range s.conns {
				if c.Crypt == nil || c.State() != connection.StateActive || c.Crypt.Mode() != mode {
					continue
				}
				tryCount++
				if err := c.Crypt.Decrypt(plain, data); err == nil {
					senderSession = sid
					senderCrypt = c.Crypt
					plain = plain[:len(data)-c.Crypt.Overhead()]
					goto found
				} else {
					lastErr = err
				}
			}
		}
	found:
		s.connMu.RUnlock()

		if senderSession == 0 && voiceDebug {
			errStr := ""
			if lastErr != nil {
				errStr = lastErr.Error()
			}
			// Down-level to Debug when another port from the same host is already mapped
			// (e.g. Mumble client's probe port vs voice port) to reduce log noise.
			downLevel := false
			if host, _, err := net.SplitHostPort(addrKey); err == nil {
				s.addrBySession.Range(func(_, v interface{}) bool {
					if a, ok := v.(net.Addr); ok {
						if h, _, e := net.SplitHostPort(a.String()); e == nil && h == host {
							downLevel = true
							return false // stop iteration
						}
					}
					return true
				})
			}
			args := []interface{}{
				"addr", addrKey, "data_len", len(data),
				"conn_count", connCount, "tries", tryCount, "last_err", errStr}
			if downLevel {
				slog.Debug("[VOICE-DEBUG] HandleUDP: trial decrypt failed, no matching session (same host, different port)", args...)
			} else {
				slog.Warn("[VOICE-DEBUG] HandleUDP: trial decrypt failed, no matching session", args...)
			}
		}
	}

	if senderSession == 0 {
		return
	}

	if voiceDebug && !fromCache {
		slog.Info("[VOICE-DEBUG] HandleUDP: session identified via trial-decrypt",
			"addr", addrKey, "session", senderSession)
	}

	if len(plain) < 1 {
		return
	}

	s.connMu.RLock()
	senderConn := s.conns[senderSession]
	s.connMu.RUnlock()
	if senderConn == nil || senderConn.Crypt != senderCrypt || senderConn.State() != connection.StateActive {
		return
	}
	senderRef := cluster.SessionRef{SessionID: senderSession, Generation: senderConn.SessionGeneration()}
	s.connMu.RLock()
	if s.conns[senderSession] != senderConn || !s.registry.Active(senderRef) {
		s.connMu.RUnlock()
		return
	}
	s.addrBySession.Store(senderRef, addr)
	s.sessionByAddr.Store(addrKey, senderRef)
	s.connMu.RUnlock()
	mode := senderConn.AudioWireMode()
	if len(plain) > mumbleaudio.MaxPacketSize {
		return
	}
	isPing := mode == mumbleaudio.WireLegacy && plain[0]>>5 == 1 || mode == mumbleaudio.WireProtobuf && plain[0] == 1

	// Type 1 = UDP ping: echo back to sender for connectivity confirmation,
	// but suppress the echo if the sender's channel has mixed crypto modes
	// (this forces the client to fall back to TCP tunnel).
	if isPing {
		timestamp, extended, err := mumbleaudio.DecodePing(mode, plain)
		if err != nil {
			return
		}
		if mode == mumbleaudio.WireProtobuf {
			if extended {
				plain = edge.ProtobufPingReply(plain[1:], s.users.Count(), s.cfg.MaxUsers, s.cfg.MaxBandwidth)
			} else {
				plain = edge.AppendProtoVarintField([]byte{1}, 1, timestamp)
			}
		}
		cid, ok := s.users.ChannelID(senderSession)
		mixed := ok && s.channelHasMixedCrypto(cid)
		if mixed {
			if voiceDebug {
				slog.Info("[VOICE-DEBUG] HandleUDP: ping suppress (mixed_crypto channel)",
					"session", senderSession, "channel", cid)
			}
			return
		}
		enc := make([]byte, len(plain)+senderCrypt.Overhead())
		if err := senderCrypt.Encrypt(enc, plain); err == nil {
			s.udpConn.WriteTo(enc, addr)
			if voiceDebug {
				slog.Info("[VOICE-DEBUG] HandleUDP: ping echo sent", "session", senderSession, "addr", addrKey)
			}
		}
		return
	}

	if s.router == nil {
		return
	}
	frame, err := s.decodeAudio(senderConn, plain, "udp")
	if err != nil {
		return
	}
	_ = s.router.RouteRef(cluster.SessionRef{SessionID: senderSession, Generation: senderConn.SessionGeneration()}, frame)
}

// Audio 双格式实现已接入，控制通道默认宣告 1.5.0。
// 该版本宣告与连接级 WireMode 使用同一协议能力基线；单一来源在
// internal/edge，Local Edge 与 Remote Edge 共用同一宣告。
const ServerVersionV2 = edge.ProtocolVersionV2

const ServerVersionV1 = edge.ProtocolVersionV1 // 1.5.0 as (major<<16)|(minor<<8)|patch

// SendVoice is the common canonical-recipient handoff used by normal voice,
// VoiceTarget, listeners and loopback.
func (s *Server) SendVoice(sender cluster.SessionRef, frame mumbleaudio.Frame, recipients []audio.Recipient) error {
	senderSessionID := sender.SessionID
	if !s.registry.Active(sender) {
		return nil
	}
	batch := cluster.VoiceBatch{Sender: sender, Frame: frame, Recipients: make([]cluster.VoiceRecipient, 0, len(recipients))}
	for _, recipient := range recipients {
		ref := recipient.Session
		if !s.registry.Active(ref) {
			continue
		}
		d := recipient.Delivery
		hasPosition := d.HasPosition
		if hasPosition {
			speaker, speakerOK := s.users.Snapshot(senderSessionID)
			target, targetOK := s.users.Snapshot(recipient.SessionID)
			hasPosition = speakerOK && targetOK && bytes.Equal(speaker.PluginContext, target.PluginContext)
		}
		forceTunnel := false
		if target, ok := s.users.Snapshot(recipient.SessionID); ok {
			forceTunnel = s.channelHasMixedCrypto(target.ChannelID)
		}
		batch.Recipients = append(batch.Recipients, cluster.VoiceRecipient{Session: ref, Context: d.Context, Volume: d.VolumeAdjustment, HasPosition: hasPosition, ForceTunnel: forceTunnel})
	}
	s.dispatcher.DispatchVoice(context.Background(), batch)
	return nil
}
func (s *Server) sendDeliveryTo(sessionID uint32, c *connection.Conn, addr interface{}, d mumbleaudio.Delivery, cache *mumbleaudio.EncodingCache) error {
	if c == nil {
		return nil
	}
	if d.HasPosition {
		speaker, ok := s.users.Snapshot(d.SenderSession)
		recipient, found := s.users.Snapshot(sessionID)
		d.HasPosition = ok && found && bytes.Equal(speaker.PluginContext, recipient.PluginContext)
	}
	packet, err := cache.Encode(c.AudioWireMode(), d)
	if err != nil {
		return err
	}
	return s.sendAudioTo(sessionID, c, addr, packet)
}

// sendAudioTo 使用已固定的连接和地址，发送时不再按可复用的 session 查找。
func (s *Server) sendAudioTo(sessionID uint32, c *connection.Conn, recipientAddr interface{}, packet []byte) error {
	if c == nil {
		if s.voiceDebug.Load() {
			slog.Warn("[VOICE-DEBUG] SendAudio: recipient conn not found", "recipient", sessionID)
		}
		return nil
	}

	voiceDebug := s.voiceDebug.Load()

	// Try UDP first if recipient has a known UDP address,
	// but force TCP tunnel when the channel has mixed crypto modes.
	if recipientAddr != nil && c.Crypt != nil && s.udpConn != nil {
		cid, uOk := s.users.ChannelID(sessionID)
		mixed := uOk && s.channelHasMixedCrypto(cid)
		if !uOk || !mixed {
			addr := recipientAddr.(net.Addr)
			overhead := c.Crypt.Overhead()
			enc := make([]byte, len(packet)+overhead)
			encErr := c.Crypt.Encrypt(enc, packet)
			if encErr == nil {
				_, writeErr := s.udpConn.WriteTo(enc, addr)
				if voiceDebug {
					slog.Info("[VOICE-DEBUG] SendAudio via UDP",
						"recipient", sessionID, "addr", addr.String(),
						"pkt_len", len(packet), "enc_len", len(enc), "err", writeErr)
				}
				return writeErr
			}
			if voiceDebug {
				slog.Error("[VOICE-DEBUG] SendAudio encrypt failed", "recipient", sessionID, "err", encErr)
			}
		} else if voiceDebug {
			slog.Info("[VOICE-DEBUG] SendAudio skipping UDP (mixed_crypto)",
				"recipient", sessionID, "mixed", mixed)
		}
	} else if voiceDebug {
		slog.Info("[VOICE-DEBUG] SendAudio no UDP path",
			"recipient", sessionID,
			"has_addr", recipientAddr != nil,
			"has_crypt", c.Crypt != nil,
			"has_udp_conn", s.udpConn != nil)
	}

	// TCP fallback: wrap as UDPTunnel message
	err := c.WriteRaw(protocol.MessageUDPTunnel, packet)
	if voiceDebug {
		slog.Info("[VOICE-DEBUG] SendAudio via TCP fallback",
			"recipient", sessionID, "pkt_len", len(packet), "err", err)
	}
	return err
}

func (s *Server) canSenderSpeak(sessionID uint32) bool {
	ref, active := s.registry.Ref(sessionID)
	if !active || !s.registry.Active(ref) {
		return false
	}
	g, ok := s.users.SpeakGateFor(sessionID)
	if !ok {
		return false
	}
	if g.Mute || g.Suppress || g.SelfMute {
		return false
	}
	return true
}

func (s *Server) canSenderSpeakInChannel(sessionID, channelID uint32) bool {
	g, ok := s.users.SpeakGateFor(sessionID)
	if !ok || g.Mute || g.Suppress || g.SelfMute {
		return false
	}
	return s.aclCheck(acl.Subject{SessionID: sessionID, UserID: g.UserID}, channelID, mumble.PermissionSpeak)
}

func (s *Server) audioFilterRecipient(senderSessionID, recipientSessionID uint32) bool {
	ref, active := s.registry.Ref(recipientSessionID)
	if !active || !s.registry.Active(ref) {
		return false
	}
	vs, ok := s.users.VoiceState(recipientSessionID)
	if !ok {
		return false
	}
	return !vs.Deaf && !vs.SelfDeaf
}

// UpdateChannelCrypto recomputes the aggregate crypto mode string for a channel.
// Must be called whenever the channel's user set changes (join, leave, move, disconnect).
func (s *Server) UpdateChannelCrypto(channelID uint32) {
	users := s.users.SnapshotByChannel(channelID)
	modes := make(map[string]bool)
	for _, u := range users {
		if u.CryptoMode != "" {
			modes[u.CryptoMode] = true
		}
	}

	var mode string
	switch len(modes) {
	case 0:
		mode = ""
	case 1:
		for m := range modes {
			mode = m
		}
	default:
		mode = "mixed"
	}

	s.channelCryptoMu.Lock()
	if mode == "" {
		delete(s.channelCrypto, channelID)
	} else {
		s.channelCrypto[channelID] = mode
	}
	s.channelCryptoMu.Unlock()
}

// channelHasMixedCrypto returns true if the channel (or any of its linked channels)
// has clients using different crypto modes.
func (s *Server) channelHasMixedCrypto(channelID uint32) bool {
	allModes := make(map[string]bool)

	channelIDs := s.chans.ConnectedChannelIDs(channelID)

	s.channelCryptoMu.RLock()
	for _, cid := range channelIDs {
		if m, ok := s.channelCrypto[cid]; ok && m != "" {
			if m == "mixed" {
				s.channelCryptoMu.RUnlock()
				return true
			}
			allModes[m] = true
		}
	}
	s.channelCryptoMu.RUnlock()
	return len(allModes) > 1
}

// ChannelCryptoMode returns the aggregate crypto mode string for a channel.
// Returns "legacy", "lite", "secure", "mixed", or "" (no active users).
func (s *Server) ChannelCryptoMode(channelID uint32) string {
	s.channelCryptoMu.RLock()
	defer s.channelCryptoMu.RUnlock()
	return s.channelCrypto[channelID]
}

// AllChannelCryptoModes returns a snapshot of all channel crypto modes.
func (s *Server) AllChannelCryptoModes() map[uint32]string {
	s.channelCryptoMu.RLock()
	defer s.channelCryptoMu.RUnlock()
	out := make(map[uint32]string, len(s.channelCrypto))
	for k, v := range s.channelCrypto {
		out[k] = v
	}
	return out
}

// NewServer creates a Mumble protocol server.
func NewServer(cfg *config.Config, db *gorm.DB, serverID uint, udpConn net.PacketConn) *Server {
	users := user.NewManager(db, cfg.MaxUsers)
	chans := channel.NewManager(db, serverID)
	_ = acl.EnsureDefaultRootACLs(db, serverID)
	registry := cluster.NewRegistry()
	_ = registry.RegisterEdge(cluster.LocalEdgeID, 1)
	s := &Server{
		cfg:           cfg,
		db:            db,
		users:         users,
		chans:         chans,
		bans:          ban.NewManager(db, serverID),
		acl:           acl.NewEvaluator(db, chans, users),
		table:         protocol.NewHandlerTable(),
		conns:         make(map[uint32]*connection.Conn),
		channelCrypto: make(map[uint32]string),
		udpConn:       udpConn,
		registry:      registry,
	}
	s.dispatcher = cluster.NewDispatcher(registry)
	s.dispatcher.RegisterVoiceTransport(cluster.LocalEdgeID, localVoiceTransport{s: s})
	s.control = cluster.NewControlTransport(registry)
	s.control.SetFailureHandler(s.controlFailed)
	if strings.EqualFold(cfg.AuthMode, "external") {
		s.authority, s.authorityErr = identity.NewExternalHTTPAuthority(identity.ExternalHTTPConfig{
			BaseURL: cfg.ExternalAuthURL, ServiceToken: cfg.ExternalAuthServiceToken,
			AuthenticatePath: cfg.ExternalAuthAuthenticatePath, ResolvePath: cfg.ExternalAuthResolvePath,
			Timeout: cfg.ExternalAuthTimeout, CACertPath: cfg.ExternalAuthCACertPath,
			ClientCertPath: cfg.ExternalAuthClientCertPath, ClientKeyPath: cfg.ExternalAuthClientKeyPath,
		})
	} else {
		s.authority = &localAuthority{server: s}
	}
	voiceDebug := cfg.VoiceDebug
	s.voiceDebug.Store(voiceDebug)
	s.SetContentPolicy(cfg.AllowRecording, cfg.MaxTextMessageLength, cfg.MaxImageMessageLength)
	s.router = audio.NewRouterWithConfig(audio.RouterConfig{
		Sender: s,
		GetChan: func(sid uint32) uint32 {
			cid, ok := s.users.ChannelID(sid)
			if !ok {
				return 0
			}
			return cid
		},
		GetUsersInChan:     s.users.SessionIDsInChannel,
		GetListenersInChan: s.listeners.SessionIDsIn,
		GetListenerVolume:  s.listeners.Volume,
		ResolveVoiceTarget: s.resolveVoiceTargetCanonical,
		ResolveSession:     s.registry.Ref,
		GetLinkedChans: func(cid uint32) []uint32 {
			ids := s.chans.ConnectedChannelIDs(cid)
			out := make([]uint32, 0, len(ids))
			for _, id := range ids {
				if id != cid {
					out = append(out, id)
				}
			}
			return out
		},
		FilterRecipient:      s.audioFilterRecipient,
		CanSenderSpeak:       s.canSenderSpeak,
		CanSenderSpeakInChan: s.canSenderSpeakInChannel,
		VoiceDebug:           voiceDebug,
	})
	s.registerHandlers()
	return s
}

// SetUDPConn binds the UDP socket the edge-local UDP ingress writes replies
// through. The shared client runtime owns the socket; this is called once
// during composition, before Run admits any traffic.
func (s *Server) SetUDPConn(conn net.PacketConn) {
	s.udpConn = conn
}

func (s *Server) registerHandlers() {
	s.edgeIngress = protocol.NewHandlerTable()
	s.coreIngress = protocol.NewHandlerTable()
	// register mirrors one registration into the ownership split and the
	// legacy union table so HandlerTable consumers keep their behavior.
	register := func(ownership edge.MessageOwnership, msgType protocol.MessageType, h protocol.MessageHandler) {
		s.table[msgType] = h
		if ownership == edge.OwnershipCore {
			s.coreIngress[msgType] = h
		} else {
			s.edgeIngress[msgType] = h
		}
	}
	// Local Edge entries: version/wire-mode negotiation, authentication (runs
	// the CryptSetup handshake) and UDP tunnel audio decode keep the raw
	// *connection.Conn because they are edge-local by protocol ownership. The
	// fused split-owned halves (Version metadata, Authenticate decision) live
	// here because the Core is in-process.
	register(edge.OwnershipSplit, protocol.MessageVersion, s.handleVersion)
	register(edge.OwnershipSplit, protocol.MessageAuthenticate, s.handleAuthenticate)
	// Ping terminates at the Local Edge (connection.Conn) and never reaches
	// this table; only the TCP latency metric is reported to Core.
	register(edge.OwnershipEdge, protocol.MessageCryptSetup, s.handleCryptSetup)
	register(edge.OwnershipSplit, protocol.MessageUDPTunnel, s.handleUDPTunnel)
	// Core control handlers go through the typed Peer adapter.
	register(edge.OwnershipCore, protocol.MessageUserRemove, s.peerAdapt(s.handleUserRemove))
	register(edge.OwnershipCore, protocol.MessageUserState, s.peerAdapt(s.handleUserState))
	register(edge.OwnershipCore, protocol.MessageChannelState, s.peerAdapt(s.handleChannelState))
	register(edge.OwnershipCore, protocol.MessageChannelRemove, s.peerAdapt(s.handleChannelRemove))
	register(edge.OwnershipCore, protocol.MessageTextMessage, s.peerAdapt(s.handleTextMessage))
	register(edge.OwnershipCore, protocol.MessageVoiceTarget, s.peerAdapt(s.handleVoiceTarget))
	register(edge.OwnershipCore, protocol.MessageBanList, s.peerAdapt(s.handleBanList))
	register(edge.OwnershipCore, protocol.MessageACL, s.peerAdapt(s.handleACL))
	register(edge.OwnershipCore, protocol.MessagePermissionQuery, s.peerAdapt(s.handlePermissionQuery))
	register(edge.OwnershipCore, protocol.MessageRequestBlob, s.peerAdapt(s.handleRequestBlob))
	register(edge.OwnershipCore, protocol.MessageUserStats, s.peerAdapt(s.handleUserStats))
	register(edge.OwnershipCore, protocol.MessageQueryUsers, s.peerAdapt(s.handleQueryUsers))
	register(edge.OwnershipCore, protocol.MessageUserList, s.handleUserList)
	register(edge.OwnershipCore, protocol.MessageContextActionModify, s.handleContextActionModify)
	register(edge.OwnershipCore, protocol.MessageContextAction, s.handleContextAction)
	register(edge.OwnershipCore, protocol.MessagePluginDataTransmission, s.handlePluginDataTransmission)
}

// HandlerTable returns the union handler table (Local Edge entries plus Core
// control entries); the ingress router routes by ownership instead.
func (s *Server) HandlerTable() protocol.HandlerTable {
	return s.table
}

// EdgeIngressHandler returns the Local Edge half of the client ingress
// boundary: handlers for edge-owned and split-owned messages that operate on
// the raw *connection.Conn transport state.
func (s *Server) EdgeIngressHandler() protocol.MessageHandler {
	return tableHandler(s.edgeIngress)
}

// CoreIngressHandler returns the Core half of the client ingress boundary:
// core-owned business messages entered through the typed Peer adapter.
func (s *Server) CoreIngressHandler() protocol.MessageHandler {
	return tableHandler(s.coreIngress)
}

func tableHandler(t protocol.HandlerTable) protocol.MessageHandler {
	return func(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
		return t.Dispatch(msgType, payload, ctx)
	}
}

// Registry returns the session registry contract; the core-mode Remote Edge
// server capability registers its edges here.
func (s *Server) Registry() *cluster.Registry { return s.registry }

// ControlTransport returns the per-session FIFO control transport contract.
func (s *Server) ControlTransport() *cluster.ControlTransport { return s.control }

// Dispatcher returns the edge-grouped voice dispatcher contract.
func (s *Server) Dispatcher() *cluster.Dispatcher { return s.dispatcher }

// UserManager returns the user manager.
// ChanManager returns the channel manager for REST API channel CRUD.
func (s *Server) ChanManager() *channel.Manager {
	return s.chans
}

func (s *Server) UserManager() *user.Manager {
	return s.users
}

// ACLEvaluator returns the ACL evaluator for cache invalidation.
func (s *Server) ACLEvaluator() *acl.Evaluator {
	return s.acl
}

// RefreshSuppressStates recomputes Suppress from Speak ACL for every connected user
// and broadcasts any changes. Call after ACL edits so clients (including Mumla/Plumble)
// see the correct suppressed indicator without rejoining. Sends a minimal
// {session, suppress} delta per murmur's clearACLCache (Server.cpp:2187-2199), with
// the real suppress value rather than upstream's hardcoded true, which is a known
// upstream bug.
//
// Note: Humla's AudioHandler treats a suppress-only update as authoritative for the
// mute OR of mute/self_mute/suppress (absent fields read as false). That is a client
// bug; we still match murmur's minimal delta rather than over-broadcasting self_mute.
// See docs/features/0007-userstate-field-presence.md.
func (s *Server) RefreshSuppressStates() {
	if s.acl == nil {
		return
	}
	for _, u := range s.users.SnapshotAll() {
		sid := u.SessionID
		// Resolved before UpdateUser — see maySpeak.
		maySpeak := s.maySpeak(acl.SubjectOf(u), u.ChannelID)
		var changed bool
		updated, ok := s.users.UpdateUser(sid, func(live *mumble.User) {
			changed = applySuppressFromSpeak(live, maySpeak)
		})
		if !ok || !changed {
			continue
		}
		s.Broadcast(0, protocol.MessageUserState, &messages.UserState{
			Session:   sid,
			Suppress:  updated.Suppress,
			SetFields: messages.UserStateSetSession | messages.UserStateSetSuppress,
		})
	}
}

// RefreshEnterStates sends receiver-specific channel lock state to every active
// client. ACL edits are rare, so a full refresh is preferred over attempting to
// infer all descendants affected by inheritance, groups, or ApplySubs.
func (s *Server) RefreshEnterStates() {
	if s == nil || s.users == nil || s.chans == nil {
		return
	}
	channels := s.chans.GetTree()
	restricted := s.enterRestrictedChannels()
	for _, u := range s.users.SnapshotAll() {
		s.refreshEnterStatesFor(u, channels, restricted)
	}
}

// RefreshEnterStatesFor refreshes lock state for one session. Channel moves can
// change @in/@out/@sub membership for that user without changing any ACL row.
func (s *Server) RefreshEnterStatesFor(sessionID uint32) {
	if s == nil || s.users == nil || s.chans == nil {
		return
	}
	u, ok := s.users.Snapshot(sessionID)
	if !ok {
		return
	}
	s.refreshEnterStatesFor(u, s.chans.GetTree(), s.enterRestrictedChannels())
}

func (s *Server) refreshEnterStatesFor(u mumble.User, channels []*mumble.Channel, restricted map[uint32]bool) {
	ref := cluster.SessionRef{SessionID: u.SessionID, Generation: u.SessionGeneration}
	subject := acl.SubjectOf(u)
	for _, ch := range channels {
		_ = s.control.Send(ref, cluster.ControlMessage{
			Type:    protocol.MessageChannelState,
			Message: s.channelToStateFor(ch, subject, restricted[ch.ID]),
		})
	}
}

func (s *Server) enterRestrictedChannels() map[uint32]bool {
	if s == nil || s.acl == nil {
		return map[uint32]bool{}
	}
	restricted, err := s.acl.EnterRestrictedChannels()
	if err != nil {
		slog.Warn("加载频道 Enter 限制失败", "error", err)
		return map[uint32]bool{}
	}
	return restricted
}

// baselinePermissions is what this server answers with when it has no ACL
// evaluator at all, so an evaluator-less server is neither wide open nor completely
// locked down: ordinary participation is allowed, administration is not.
const baselinePermissions = acl.DefaultPermissions

// aclCheck resolves a permission for a subject in channelID. Every permission gate
// in this package goes through here rather than touching s.acl directly, so the
// no-evaluator case is answered in exactly one place.
//
// Lock ordering: resolve ACL questions BEFORE taking the user manager's lock. The
// evaluator can look up connected users for group membership, so calling it from
// inside UpdateUser would deadlock against that same lock.
func (s *Server) aclCheck(subject acl.Subject, channelID uint32, perm mumble.Permission) bool {
	if s == nil || s.acl == nil {
		return baselinePermissions&perm == perm
	}
	return s.acl.Check(subject, channelID, perm)
}

// aclPermissions returns the effective permission mask for a subject in channelID.
func (s *Server) aclPermissions(subject acl.Subject, channelID uint32) uint32 {
	if s == nil || s.acl == nil {
		return uint32(baselinePermissions)
	}
	return s.acl.EffectivePermissions(subject, channelID)
}

// invalidateACLCache drops cached permissions. Group membership depends on where a
// user currently is (@in/@out) and on the tokens they presented, so every change to
// either has to go through here.
func (s *Server) invalidateACLCache() {
	if s == nil || s.acl == nil {
		return
	}
	s.acl.InvalidateCache()
}

// maySpeak reports whether the subject may Speak in channelID.
func (s *Server) maySpeak(subject acl.Subject, channelID uint32) bool {
	return s.aclCheck(subject, channelID, mumble.PermissionSpeak)
}

// channelIsFull reports whether a channel has reached its user limit for the
// subject, mirroring murmur's Server::isChannelFull (Server.cpp:2450): a Write
// holder is never blocked, and a limit of 0 means unlimited.
func (s *Server) channelIsFull(channelID uint32, subject acl.Subject) bool {
	ch, ok := s.chans.GetChannel(channelID)
	if !ok || ch.MaxUsers == 0 {
		return false
	}
	if s.aclCheck(subject, channelID, mumble.PermissionWrite) {
		return false
	}
	return uint32(s.users.CountInChannel(channelID)) >= ch.MaxUsers
}

// firstPermanentChannel walks up from channelID to the nearest ancestor that is not
// a temporary channel, mirroring the loop murmur uses before honouring a mute in a
// temporary channel (Messages.cpp:815-833). Reports false when the whole chain is
// temporary or the channel is unknown.
func (s *Server) firstPermanentChannel(channelID uint32) (uint32, bool) {
	if s.chans == nil {
		return channelID, true
	}
	for _, cid := range s.chans.AncestorChain(channelID) {
		ch, ok := s.chans.GetChannel(cid)
		if !ok {
			return 0, false
		}
		if !ch.IsTemporary {
			return cid, true
		}
	}
	return 0, false
}

// SetContentPolicy updates the recording policy and content size limits. Callable
// at runtime when server config is updated via REST.
func (s *Server) SetContentPolicy(allowRecording bool, maxTextBytes, maxImageBytes int) {
	s.allowRecording.Store(allowRecording)
	s.maxTextBytes.Store(int64(maxTextBytes))
	s.maxImageBytes.Store(int64(maxImageBytes))
}

// recordingAllowed reports whether clients may announce that they are recording.
func (s *Server) recordingAllowed() bool {
	return s.allowRecording.Load()
}

// maxTextLength returns the cap on user-supplied text (chat messages, comments).
// 0 means unlimited.
func (s *Server) maxTextLength() int {
	return int(s.maxTextBytes.Load())
}

// maxImageLength returns the cap on user-supplied image payloads (avatar textures).
// 0 means unlimited.
func (s *Server) maxImageLength() int {
	return int(s.maxImageBytes.Load())
}

// BanManager returns the ban manager (for cache invalidation when bans change via REST).
func (s *Server) BanManager() *ban.Manager {
	return s.bans
}

// HasUDPAddress returns true if the session has sent at least one UDP packet (voice or ping),
// indicating the client is using native UDP transport rather than TCP tunnel.
func (s *Server) HasUDPAddress(sessionID uint32) bool {
	ref, found := s.registry.Ref(sessionID)
	if !found {
		return false
	}
	_, ok := s.addrBySession.Load(ref)
	return ok
}

// SetVoiceDebug enables or disables voice path debug logging (UDP, TCP tunnel, routing).
// Callable at runtime when server config is updated via REST.
func (s *Server) SetVoiceDebug(enabled bool) {
	s.voiceDebug.Store(enabled)
	if s.router != nil {
		s.router.SetVoiceDebug(enabled)
	}
}

// BroadcastChannelState broadcasts a channel's state to all connected clients.
// Used when channels are created/updated via REST so Mumble clients see the changes.
func (s *Server) BroadcastChannelState(ch *mumble.Channel) {
	if ch == nil || s.users == nil {
		return
	}
	restricted := s.enterRestrictedChannels()[ch.ID]
	for _, snap := range s.registry.Sessions() {
		if !deliverableState(snap.State) {
			continue
		}
		u, ok := s.users.Snapshot(snap.Ref.SessionID)
		if !ok {
			continue
		}
		_ = s.control.Send(snap.Ref, cluster.ControlMessage{
			Type:    protocol.MessageChannelState,
			Message: s.channelToStateFor(ch, acl.SubjectOf(u), restricted),
		})
	}
}

// BroadcastChannelRemove broadcasts a channel removal to all connected clients.
func (s *Server) BroadcastChannelRemove(channelID uint32) {
	s.Broadcast(0, protocol.MessageChannelRemove, &messages.ChannelRemove{ChannelID: channelID})
}

// registerConnOnly maps a connection for control/UDP routing without any
// lifecycle action. The authentication path owns the session lifecycle (Bind
// and BeginSync ran before sendSync) and must never resurrect a session that
// disconnect cleanup has already closed: re-binding here would re-announce a
// ghost and block session-ID reuse.
func (s *Server) registerConnOnly(sessionID uint32, c *connection.Conn) {
	s.connMu.Lock()
	s.conns[sessionID] = c
	s.connMu.Unlock()
}

// RegisterConn registers a connection for broadcasting.
func (s *Server) RegisterConn(sessionID uint32, c *connection.Conn) {
	s.connMu.Lock()
	s.conns[sessionID] = c
	s.connMu.Unlock()
	// Compatibility for test/local callers that register an already-created
	// user directly. The authentication path binds Syncing before this call and
	// drives its own barrier, so only pre-registered conns take this path.
	if _, exists := s.registry.Ref(sessionID); !exists {
		if u, ok := s.users.Snapshot(sessionID); ok {
			c.SetSessionGeneration(u.SessionGeneration)
			ref := cluster.SessionRef{SessionID: sessionID, Generation: u.SessionGeneration}
			if s.registry.Bind(ref, cluster.LocalEdgeID) == nil {
				if s.control.BeginSync(ref, controlSink{conn: c}, controlDeferredLimit) == nil {
					_ = s.control.CommitSyncAndAnnounce(context.Background(), ref, func() {})
				} else {
					_ = s.registry.Unbind(ref)
				}
			}
		}
	}
}

// UnregisterConn tears down a session's bindings and reports whether the
// session was ever announced to other clients; a never-announced session must
// not produce a UserRemove broadcast. Generation-aware: a stale SessionRef
// (its ID already reused by a newer logical session) touches nothing — a
// delayed disconnect must never clean up its successor. Callers must call
// UpdateChannelCrypto for the user's channel after removing the user
// (UnregisterConn cannot see the user if callers remove first).
func (s *Server) UnregisterConn(ref cluster.SessionRef) bool {
	if !ref.Valid() {
		return false
	}
	announced := false
	_ = s.registry.CloseAndUnbind(ref, func(snap cluster.SessionSnapshot) {
		announced = snap.Announced
		// Detach the control queue without closing the socket: connection
		// teardown is owned by SendThenClose, the read loop exit, or the
		// caller's own cleanup.
		s.control.Detach(ref)
		if a, ok := s.addrBySession.Load(ref); ok {
			s.sessionByAddr.CompareAndDelete(a.(net.Addr).String(), ref)
		}
		s.connMu.Lock()
		delete(s.conns, ref.SessionID)
		s.removeVoiceTargetsLocked(ref.SessionID)
		s.connMu.Unlock()
		s.addrBySession.Delete(ref)
		s.textRateLimiter.Delete(ref.SessionID)
		s.userStateRateLimiter.Delete(ref.SessionID)
		// Listener state dies with the session (no persistence); the UserRemove
		// broadcast that follows already implies its listening list is gone.
		s.listeners.RemoveAllFor(ref.SessionID)
	})
	s.chans.CleanEmptyTempChannels(func(cid uint32) bool {
		return s.users.CountInChannel(cid) > 0
	})
	return announced
}

// KickSession kicks a user (server-initiated, e.g. from REST API). Actor 0 = server.
func (s *Server) KickSession(sessionID uint32, reason string) bool {
	u, ok := s.users.Snapshot(sessionID)
	if !ok {
		return false
	}
	return s.KickSessionRef(cluster.SessionRef{SessionID: sessionID, Generation: u.SessionGeneration}, reason)
}

// KickSessionRef 只踢出指定 generation；关闭通知与该会话解绑串行化，
// 用户删除仍在 Manager 锁内校验 generation。
func (s *Server) KickSessionRef(ref cluster.SessionRef, reason string) bool {
	u, ok := s.users.SnapshotRef(ref)
	if !ok {
		return false
	}
	channelID := u.ChannelID
	ur := &messages.UserRemove{Session: ref.SessionID, Actor: 0, Reason: reason, Ban: false}
	_, err := s.registry.BeginCloseAndNotify(ref, func(snap cluster.SessionSnapshot) {
		if _, live := s.users.SnapshotRef(ref); live && snap.Announced {
			s.Broadcast(ref.SessionID, protocol.MessageUserRemove, ur)
		}
	})
	if err != nil {
		return false
	}
	s.sendThenClose(ref, protocol.MessageUserRemove, ur)
	s.users.RemoveIfGeneration(ref.SessionID, ref.Generation)
	s.UnregisterConn(ref)
	if channelID != 0 {
		s.UpdateChannelCrypto(channelID)
	}
	slog.Info("User kicked", "session", ref.SessionID, "name", u.Name, "reason", reason)
	return true
}

// MuteSession sets server mute state for a user. Routed through the same helper as
// the protocol handler so the "un-muting clears deafen" invariant holds here too.
// Broadcasts a minimal {session, actor, mute} delta, plus deaf only when the cascade
// cleared it, matching handleUserState's echo shape rather than a full snapshot.
func (s *Server) MuteSession(sessionID uint32, mute bool) bool {
	req := &messages.UserState{
		Session:   sessionID,
		Mute:      mute,
		SetFields: messages.UserStateSetSession | messages.UserStateSetMute,
	}
	updated, ok := s.users.UpdateUser(sessionID, func(u *mumble.User) {
		applyAdminVoiceState(&u.VoiceState, req)
	})
	if !ok {
		return false
	}
	// Actor 0 = server-initiated (REST), matching KickSession/BanAndKickSession. Left
	// without presence so it is omitted on the wire rather than claiming session 0 acted.
	s.Broadcast(0, protocol.MessageUserState, req)
	slog.Info("User mute changed via REST", "session", sessionID, "name", updated.Name, "mute", mute)
	return true
}

// banEntry starts a BanList entry with the shared name/reason/start fields.
func banEntry(name, reason string) messages.BanEntry {
	return messages.BanEntry{
		Name:   name,
		Reason: reason,
		Start:  time.Now().Format(time.RFC3339),
	}
}

// banEntryByCert bans a client by certificate fingerprint so the ban survives IP changes.
func banEntryByCert(name, reason, certHash string) messages.BanEntry {
	be := banEntry(name, reason)
	be.Hash = strings.ToLower(certHash)
	return be
}

// banEntryByIP bans a client by address. IPv4 uses a /32 mask; IPv6 uses /128.
func banEntryByIP(name, reason string, ip net.IP) messages.BanEntry {
	be := banEntry(name, reason)
	mask := uint32(32)
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	} else {
		mask = 128
	}
	be.Address = ip
	be.Mask = mask
	return be
}

// appendSessionBan derives ban entries from Core user metadata — the stored
// address first, the local edge transport as fallback — so protocol- and
// REST-initiated bans behave identically for sessions without a local conn
// (remote edge sessions). Cert-hash bans survive IP changes; otherwise the
// address is banned.
//
// TODO(auth metadata): the local-conn fallback below must disappear once
// every session creation path (edge attestation included) carries
// RemoteAddress/CertHash/CertificateVerified/ClientVersion metadata; remote
// sessions already resolve bans purely from stored metadata — pinned by
// TestRemoteSessionBanRequiresNoSocketFallback.
func (s *Server) appendSessionBan(existing []messages.BanEntry, u mumble.User, reason string) []messages.BanEntry {
	var ip net.IP
	if u.Address != "" {
		ip = net.ParseIP(u.Address)
	}
	if ip == nil {
		if c := s.conn(u.SessionID); c != nil {
			if addr := c.RemoteAddr(); addr != nil {
				if host, _, err := net.SplitHostPort(addr.String()); err == nil {
					ip = net.ParseIP(host)
				}
			}
		}
	}
	if u.CertHash != "" {
		return append(existing, banEntryByCert(u.Name, reason, u.CertHash))
	}
	if ip != nil {
		return append(existing, banEntryByIP(u.Name, reason, ip))
	}
	return existing
}

// BanAndKickSession bans the user by IP and kicks them.
func (s *Server) BanAndKickSession(sessionID uint32, reason string) bool {
	u, ok := s.users.Snapshot(sessionID)
	if !ok {
		return false
	}
	existing := s.bans.List()
	if updated := s.appendSessionBan(existing, u, reason); len(updated) != len(existing) {
		_ = s.bans.Replace(updated)
	}
	channelID := u.ChannelID
	ur := &messages.UserRemove{Session: sessionID, Actor: 0, Reason: reason, Ban: true}
	ref := cluster.SessionRef{SessionID: sessionID, Generation: u.SessionGeneration}
	if s.beginCloseAnnounced(sessionID, u.SessionGeneration) {
		s.Broadcast(sessionID, protocol.MessageUserRemove, ur)
	}
	s.sendThenClose(ref, protocol.MessageUserRemove, ur)
	s.users.RemoveIfGeneration(sessionID, u.SessionGeneration)
	s.UnregisterConn(ref)
	if channelID != 0 {
		s.UpdateChannelCrypto(channelID)
	}
	slog.Info("User banned and kicked via REST", "session", sessionID, "name", u.Name, "reason", reason)
	return true
}

// Broadcast sends a message to every live session except skipSession. Active
// sessions receive immediately through the per-session FIFO; Syncing sessions
// get the message deferred by the transport until their initial sync commits,
// so no state change is lost across the snapshot race (plan 0014 §四.2.3).
func (s *Server) Broadcast(skipSession uint32, msgType protocol.MessageType, msg messages.Message) {
	for _, snap := range s.registry.Sessions() {
		if snap.Ref.SessionID == skipSession || !deliverableState(snap.State) {
			continue
		}
		_ = s.control.Send(snap.Ref, cluster.ControlMessage{Type: msgType, Message: msg})
	}
}

// sendControl delivers a per-session control message through the same FIFO as
// Broadcast: immediate when Active, deferred while Syncing, dropped otherwise.
func (s *Server) sendControl(sessionID uint32, msgType protocol.MessageType, msg messages.Message) {
	ref, ok := s.registry.Ref(sessionID)
	if !ok {
		return
	}
	_ = s.control.Send(ref, cluster.ControlMessage{Type: msgType, Message: msg})
}

// deliverableState reports whether a session may receive ordinary control
// traffic now (Active) or once its sync commits (Syncing).
func deliverableState(state cluster.SessionState) bool {
	return state == cluster.StateActive || state == cluster.StateSyncing
}

// handleVersion is a Local Edge entry: audio wire-mode negotiation is
// edge-owned transport state and must never move onto the Peer interface.
func (s *Server) handleVersion(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
	c := ctx.(*connection.Conn)
	if len(payload) > 0 {
		var v messages.Version
		if err := v.Unmarshal(payload); err != nil {
			return err
		}
		slog.Debug("client version", "release", v.Release, "os", v.OS)
		c.SetClientCryptoModes(v.CryptoModes)
		c.SetClientVersion(clientVersionFull(v))
		c.NegotiateAudioWireMode(s.ProtocolVersionV2())
		if s.voiceDebug.Load() {
			slog.Info("audio negotiation", "client_version", c.ClientVersion(), "wire_mode", c.AudioWireMode())
		}
	}
	return nil
}

// clientVersionFull 委托共享实现（edge.ClientVersionFull），Local/Remote Edge
// 使用同一解析。
func clientVersionFull(v messages.Version) uint64 {
	return edge.ClientVersionFull(v)
}

// handleAuthenticate is a Local Edge entry: it collects the client certificate
// and remote address, runs the CryptSetup handshake, and only then hands the
// logical session to Core (registry + control transport).
func (s *Server) handleAuthenticate(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
	c := ctx.(*connection.Conn)
	var authMsg messages.Authenticate
	if err := authMsg.Unmarshal(payload); err != nil {
		return err
	}
	addr := c.RemoteAddr()
	certHash := c.CertificateHash()
	if s.bans.IsBanned(addr, certHash) {
		return s.sendReject(c, messages.RejectWrongServerPW, "Banned")
	}
	if authMsg.Username == "" {
		return s.sendReject(c, messages.RejectInvalidUsername, "Username required")
	}
	if _, ok := s.users.SnapshotByName(authMsg.Username); ok {
		return s.sendReject(c, messages.RejectUsernameInUse, "Username in use")
	}
	if s.users.Count() >= s.cfg.MaxUsers && s.cfg.MaxUsers > 0 {
		return s.sendReject(c, messages.RejectServerFull, "Server full")
	}
	if s.authorityErr != nil || s.authority == nil {
		slog.Error("identity authority unavailable", "err", s.authorityErr)
		return s.sendReject(c, messages.RejectAuthenticatorFail, "Identity service unavailable")
	}
	remoteIP := ""
	if addr != nil {
		remoteIP, _, _ = net.SplitHostPort(addr.String())
		if remoteIP == "" {
			remoteIP = addr.String()
		}
	}
	authRequest := identity.AuthenticateRequest{
		ServerInstanceID: s.cfg.ExternalAuthServerInstanceID,
		Username:         authMsg.Username, Password: authMsg.Password,
		CertificateHash: certHash, RemoteIP: remoteIP,
	}
	// Password-based external identity providers cannot authenticate a request
	// that omits the password. Use Murmur's standard rejection type so clients
	// that support interactive password entry can retry with a password; this is
	// a client-input condition, not a provider outage.
	if s.authority.External() && authRequest.Password == "" {
		return s.sendReject(c, messages.RejectWrongServerPW, "Password required")
	}
	authResult, err := s.authority.Authenticate(context.Background(), authRequest)
	if err != nil {
		// Keep diagnostics useful without logging passwords or certificate values.
		slog.Warn("identity authentication failed closed", "err", err,
			"identity_username", authRequest.Username,
			"identity_password_present", authRequest.Password != "",
			"identity_server_instance_id", authRequest.ServerInstanceID,
			"identity_certificate_present", authRequest.CertificateHash != "",
			"identity_remote_ip", authRequest.RemoteIP)
		return s.sendReject(c, messages.RejectAuthenticatorFail, "Identity service unavailable")
	}
	if authResult.Decision != identity.DecisionAllow {
		return s.sendReject(c, messages.RejectWrongUserPW, "Invalid credentials")
	}
	resolved := authResult.Identity
	if !resolved.Eligible || resolved.Name == "" {
		return s.sendReject(c, messages.RejectWrongUserPW, "Invalid credentials")
	}
	u := mumble.User{
		UserID:                  resolved.UserID,
		ChannelID:               s.joinChannelFor(resolved.UserID),
		Name:                    resolved.Name,
		AccessTokens:            sanitizedClientTokens(authMsg.Tokens),
		ExternalGroups:          append([]string(nil), resolved.Groups...),
		ExternalIdentity:        s.authority.External(),
		IdentityVersion:         resolved.IdentityVersion,
		PolicyVersion:           resolved.PolicyVersion,
		IdentityLastValidatedAt: time.Now(),
		IsSuperUser:             resolved.IsSuperUser,
		// Core-owned copy of the split-owned client metadata the edge collected
		// from the Version message; Core business decisions (e.g. the legacy
		// recording announcement) read this, never the edge connection.
		ClientVersion: c.ClientVersion(),
	}
	if addr != nil {
		if host, _, err := net.SplitHostPort(addr.String()); err == nil {
			u.Address = host
		} else {
			u.Address = addr.String()
		}
	}
	u.CertHash = certHash
	u.CertificateVerified = c.CertificateVerified()
	stored, ok := s.users.Add(u)
	if !ok {
		return s.sendReject(c, messages.RejectServerFull, "Server full")
	}
	c.SetSessionID(stored.SessionID)
	c.SetSessionGeneration(stored.SessionGeneration)
	c.SetUserName(stored.Name)
	ref := cluster.SessionRef{SessionID: stored.SessionID, Generation: stored.SessionGeneration}
	if err := s.registry.Bind(ref, cluster.LocalEdgeID); err != nil {
		s.users.RemoveIfGeneration(stored.SessionID, stored.SessionGeneration)
		return s.sendReject(c, messages.RejectAuthenticatorFail, "Session setup failed")
	}
	if err := s.control.BeginSync(ref, controlSink{conn: c}, controlDeferredLimit); err != nil {
		_, _ = s.registry.BeginClose(ref)
		_ = s.registry.Unbind(ref)
		s.users.RemoveIfGeneration(stored.SessionID, stored.SessionGeneration)
		return s.sendReject(c, messages.RejectAuthenticatorFail, "Session setup failed")
	}
	// This session brings its own access tokens and channel, both of which feed
	// group resolution, so anything cached for this account is now suspect.
	s.invalidateACLCache()
	if err := s.sendSync(c, stored, ref); err != nil {
		s.closeFailedSync(ref)
		return err
	}
	// CommitSync confirms the ServerConfig write receipt, splices the deferred
	// queue ahead of new broadcasts and commits the registry to Active.
	if err := s.control.CommitSyncAndAnnounce(context.Background(), ref, func() {
		c.SetActive()
		s.Broadcast(stored.SessionID, protocol.MessageUserState, userToState(stored))
	}); err != nil {
		s.closeFailedSync(ref)
		return err
	}
	s.UpdateChannelCrypto(stored.ChannelID)
	slog.Info("Mumble client authenticated", "user", stored.Name, "session", stored.SessionID, "channel", stored.ChannelID)
	return nil
}

// joinChannelFor picks the channel a connecting user lands in: the configured
// default, falling back to root when that channel is full or cannot be entered.
// Murmur applies the same fallback chain when a client connects (Messages.cpp:300-315).
// The user has no session yet, so permissions resolve against the account alone.
func (s *Server) joinChannelFor(userID uint32) uint32 {
	subject := acl.SubjectForUserID(userID)
	root := uint32(s.chans.RootID())
	if s.cfg.DefaultChannel <= 0 {
		return root
	}
	target := uint32(s.cfg.DefaultChannel)
	if target == root {
		return root
	}
	if _, exists := s.chans.GetChannel(target); !exists {
		return root
	}
	if !s.aclCheck(subject, target, mumble.PermissionEnter) {
		return root
	}
	if s.channelIsFull(target, subject) {
		return root
	}
	return target
}

// lookupRegisteredUser returns UserID and credentials for a username on this server's registered_users.
func (s *Server) lookupRegisteredUser(username string) (userID uint32, passwordHash, certHash string, found bool) {
	var u struct {
		UserID       int32
		PasswordHash string
		CertHash     string
	}
	err := s.db.Table("registered_users").Where("server_id = ? AND name = ?", s.chans.ServerID(), username).
		Select("user_id", "password_hash", "cert_hash").First(&u).Error
	if err != nil {
		return 0, "", "", false
	}
	return uint32(u.UserID), u.PasswordHash, u.CertHash, true
}

func registeredUserCredentialsValid(storedPasswordHash, storedCertHash, providedPassword, presentedCertHash string) bool {
	certMatches := storedCertHash != "" && presentedCertHash != "" && strings.EqualFold(storedCertHash, presentedCertHash)
	passwordMatches := false
	if providedPassword != "" && storedPasswordHash != "" {
		// Support both Argon2id and legacy bcrypt hashes.
		passwordMatches = (strings.HasPrefix(storedPasswordHash, "$argon2id$") && auth.CompareArgon2id(storedPasswordHash, providedPassword)) ||
			(strings.HasPrefix(storedPasswordHash, "$2") && auth.ComparePassword(storedPasswordHash, providedPassword))
	}
	return certMatches || passwordMatches
}

func sanitizedClientTokens(tokens []string) []string {
	result := make([]string, 0, len(tokens))
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		result = append(result, token)
	}
	return result
}

// sendReject 交付 Reject 作为连接的最后一条消息并随后关闭连接；
// 与 Remote Edge 共用同一 edge.SendReject 实现（客户端行为契约）。
func (s *Server) sendReject(c *connection.Conn, typ messages.RejectType, reason string) error {
	return edge.SendReject(c, typ, reason)
}

func cryptoModeString(mode crypto.Mode) string {
	switch mode {
	case crypto.ModeSecure:
		return "secure"
	case crypto.ModeLite:
		return "lite"
	default:
		return "legacy"
	}
}

// negotiateCryptoMode selects the best mutually-supported crypto mode for the connection.
func (s *Server) negotiateCryptoMode(c *connection.Conn) crypto.Mode {
	clientModes := c.ClientCryptoModes()
	if clientModes == 0 {
		clientModes = 0x02 // standard client: legacy only
	}
	mutual := clientModes & 0x07 // server supports all (lite|legacy|secure)
	tls13 := false
	hasClientCert := false
	if tc, ok := c.Conn.(*tls.Conn); ok {
		st := tc.ConnectionState()
		tls13 = st.Version == tls.VersionTLS13
		hasClientCert = len(st.PeerCertificates) > 0
	}
	if (mutual&0x04) != 0 && tls13 && hasClientCert {
		return crypto.ModeSecure
	}
	if (mutual & 0x02) != 0 {
		return crypto.ModeLegacy
	}
	if (mutual & 0x01) != 0 {
		return crypto.ModeLite
	}
	return crypto.ModeLegacy
}

// sendSync performs the Local Edge CryptSetup and readiness, then streams the
// Core-owned initial snapshot through the session's initial lane. The
// client-visible order is unchanged: CryptSetup → CodecVersion → ChannelState
// list → self UserState → Active roster → ServerSync → ServerConfig.
func (s *Server) sendSync(c *connection.Conn, u mumble.User, ref cluster.SessionRef) error {
	mode := s.negotiateCryptoMode(c)
	if updated, ok := s.users.UpdateUser(u.SessionID, func(live *mumble.User) {
		live.CryptoMode = cryptoModeString(mode)
	}); ok {
		u = updated
	}
	c.Crypt = crypto.NewCryptState(mode)
	key, encNonce, decNonce := s.generateCryptSetup(mode)
	c.Crypt.SetKey(key, encNonce, decNonce)
	if err := c.WriteMessage(protocol.MessageCryptSetup, &messages.CryptSetup{
		Key:         key,
		ClientNonce: decNonce, // server's decrypt nonce = client's encrypt nonce
		ServerNonce: encNonce, // server's encrypt nonce = client's decrypt nonce
	}); err != nil {
		return err
	}
	// Register conn for UDP routing before rest of sync so HandleUDP can identify
	// the sender when the client sends its first UDP packet (ping/voice) after CryptSetup.
	// Map-only: if disconnect cleanup already closed this session, the detached
	// control queue makes the next SendInitial fail and aborts the sync.
	s.registerConnOnly(u.SessionID, c)
	initial := func(msgType protocol.MessageType, msg messages.Message) error {
		return s.control.SendInitial(ref, cluster.ControlMessage{Type: msgType, Message: msg})
	}
	if err := initial(protocol.MessageCodecVersion, &messages.CodecVersion{Opus: true}); err != nil {
		return err
	}
	restricted := s.enterRestrictedChannels()
	subject := acl.SubjectOf(u)
	for _, ch := range s.chans.GetTree() {
		cs := s.channelToStateFor(ch, subject, restricted[ch.ID])
		if err := initial(protocol.MessageChannelState, cs); err != nil {
			return err
		}
	}
	// Listening state rides the roster snapshots so a connecting client can render
	// every user's monitored channels; volume adjustments stay owner-only, matching
	// murmur's broadcastListenerVolumeAdjustments=false default. userToState itself
	// stays pure — the listening list is attached at this call site only.
	selfState := userToState(u)
	selfState.ListeningChannelAdd = s.listeners.ChannelsFor(u.SessionID)
	selfState.ListeningVolumeAdjustment = s.listeners.ListeningVolumes(u.SessionID)
	if err := initial(protocol.MessageUserState, selfState); err != nil {
		return err
	}
	for _, ou := range s.users.SnapshotAll() {
		if ou.SessionID != u.SessionID {
			other := cluster.SessionRef{SessionID: ou.SessionID, Generation: ou.SessionGeneration}
			if !s.registry.Active(other) {
				continue
			}
			state := userToState(ou)
			state.ListeningChannelAdd = s.listeners.ChannelsFor(ou.SessionID)
			if err := initial(protocol.MessageUserState, state); err != nil {
				return err
			}
		}
	}
	perms := uint64(s.aclPermissions(acl.SubjectOf(u), s.chans.RootID()))
	if err := initial(protocol.MessageServerSync, &messages.ServerSync{
		Session:      u.SessionID,
		MaxBandwidth: uint32(s.cfg.MaxBandwidth),
		WelcomeText:  s.cfg.WelcomeText,
		Permissions:  perms,
	}); err != nil {
		return err
	}
	return initial(protocol.MessageServerConfig, &messages.ServerConfig{
		MaxBandwidth:       uint32(s.cfg.MaxBandwidth),
		WelcomeText:        s.cfg.WelcomeText,
		AllowHTML:          true,
		MessageLength:      uint32(s.maxTextLength()),
		ImageMessageLength: uint32(s.maxImageLength()),
		MaxUsers:           uint32(s.cfg.MaxUsers),
		RecordingAllowed:   s.recordingAllowed(),
	})
}

func (s *Server) generateCryptSetup(mode crypto.Mode) (key, encNonce, decNonce []byte) {
	switch mode {
	case crypto.ModeSecure:
		key = make([]byte, 32)
		rand.Read(key)
		encNonce = make([]byte, 12)
		rand.Read(encNonce)
		decNonce = make([]byte, 12)
		rand.Read(decNonce)
		return key, encNonce, decNonce
	case crypto.ModeLite:
		return []byte{}, nil, nil
	default:
		key = make([]byte, 16)
		rand.Read(key)
		encNonce = make([]byte, 16)
		rand.Read(encNonce)
		decNonce = make([]byte, 16)
		rand.Read(decNonce)
		return key, encNonce, decNonce
	}
}

func channelToState(ch *mumble.Channel) *messages.ChannelState {
	// Root (ID 0): omit parent field on wire. Murmur does the same — the proto2
	// `optional` parent field is absent for root so has_parent()=false on the client.
	hasParent := ch.ID != 0
	return &messages.ChannelState{
		ChannelID:   ch.ID,
		Parent:      ch.ParentID,
		HasParent:   hasParent,
		Name:        ch.Name,
		Description: ch.Description,
		Position:    ch.Position,
		MaxUsers:    ch.MaxUsers,
		Temporary:   ch.IsTemporary,
		Links:       ch.Links,
	}
}

func (s *Server) channelToStateFor(ch *mumble.Channel, subject acl.Subject, restricted bool) *messages.ChannelState {
	state := channelToState(ch)
	state.IsEnterRestricted = restricted
	state.HasEnterRestricted = true
	state.CanEnter = s.aclCheck(subject, ch.ID, mumble.PermissionEnter)
	state.HasCanEnter = true
	return state
}

// userToState builds a join/roster snapshot of a user for broadcast, mirroring
// murmur's transmission of a user's profile to a newly connecting client
// (Messages.cpp:493-530). This is a snapshot, not a delta: field presence here means
// "this flag is set", not "this flag just changed", so voice flags are emitted only
// when true. mute/deaf and self_mute/self_deaf are mutually exclusive on the wire
// (a deafened user does not also carry an explicit mute=true), matching murmur's
// `if (deaf) ... else if (mute) ...` exactly.
//
// This must never be used to build the echo for a client-initiated UserState update
// (handleUserState) or any other delta broadcast: those broadcast the mutated inbound
// message instead, per docs/architecture/protocol-encoding.md, so that presence
// continues to mean "changed" on that path. Using a snapshot there was the v0.1.4
// regression this function's shape now guards against.
//
// Name and UserID are included whenever non-empty/non-zero (typical for connected
// users) because these snapshots are meant to be self-contained. Texture and Comment
// keep omit-if-empty encoding and do not set presence bits, so a routine mute update
// never looks like a blob clear. PluginIdentity and PluginContext are never sent to
// clients (per Mumble.proto).
func userToState(u mumble.User) *messages.UserState {
	state := &messages.UserState{
		Session:   u.SessionID,
		UserID:    u.UserID,
		Name:      u.Name,
		ChannelID: u.ChannelID,
		Texture:   u.Texture,
		Comment:   u.Comment,
		SetFields: messages.UserStateSetSession | messages.UserStateSetChannelID,
	}
	if u.Deaf {
		state.Deaf = true
		state.SetFields |= messages.UserStateSetDeaf
	} else if u.Mute {
		state.Mute = true
		state.SetFields |= messages.UserStateSetMute
	}
	if u.Suppress {
		state.Suppress = true
		state.SetFields |= messages.UserStateSetSuppress
	}
	if u.PrioritySpeaker {
		state.PrioritySpeaker = true
		state.SetFields |= messages.UserStateSetPrioritySpeaker
	}
	if u.Recording {
		state.Recording = true
		state.SetFields |= messages.UserStateSetRecording
	}
	if u.SelfDeaf {
		state.SelfDeaf = true
		state.SetFields |= messages.UserStateSetSelfDeaf
	} else if u.SelfMute {
		state.SelfMute = true
		state.SetFields |= messages.UserStateSetSelfMute
	}
	return state
}

func (s *Server) handleUserRemove(msgType protocol.MessageType, payload []byte, c Peer) error {
	if !s.peerActive(c) {
		return nil
	}
	var ur messages.UserRemove
	if err := ur.Unmarshal(payload); err != nil {
		return err
	}
	if ur.Session == 0 {
		return nil
	}
	sender, ok := s.users.Snapshot(c.SessionID())
	if !ok {
		return nil
	}
	target, ok := s.users.Snapshot(ur.Session)
	if !ok {
		return nil
	}
	perm := mumble.PermissionKick
	if ur.Ban {
		perm = mumble.PermissionBan
	}
	// SuperUser can never be kicked or banned, by anybody (murmur Messages.cpp:1245).
	if target.IsSuperUser || !s.aclCheck(acl.SubjectOf(sender), s.chans.RootID(), perm) {
		_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{
			Type: messages.DenyPermission, Reason: "No " + map[bool]string{false: "kick", true: "ban"}[ur.Ban] + " permission",
		})
		return nil
	}
	ur.Actor = c.SessionID()
	if ur.Ban {
		existing := s.bans.List()
		if updated := s.appendSessionBan(existing, target, ur.Reason); len(updated) != len(existing) {
			_ = s.bans.Replace(updated)
		}
	}
	channelID := target.ChannelID
	targetRef := cluster.SessionRef{SessionID: ur.Session, Generation: target.SessionGeneration}
	if s.beginCloseAnnounced(ur.Session, target.SessionGeneration) {
		s.Broadcast(ur.Session, protocol.MessageUserRemove, &ur)
	}
	s.sendThenClose(targetRef, protocol.MessageUserRemove, &ur)
	s.users.RemoveIfGeneration(ur.Session, target.SessionGeneration)
	s.UnregisterConn(targetRef)
	if channelID != 0 {
		s.UpdateChannelCrypto(channelID)
	}
	return nil
}

// beginCloseAnnounced transitions a session toward Closing and reports whether
// it was ever announced to other clients. The UserRemove broadcast is emitted
// only when true: a victim still mid-sync was never seen by anybody, so its
// removal must stay silent (plan 0014 §四.4).
func (s *Server) beginCloseAnnounced(sessionID uint32, generation uint64) bool {
	snap, err := s.registry.BeginClose(cluster.SessionRef{SessionID: sessionID, Generation: generation})
	if err != nil {
		return false
	}
	return snap.Announced
}

// selfOnlyUserStateFields are UserState fields a client may only set on itself.
const selfOnlyUserStateFields = messages.UserStateSetSelfMute | messages.UserStateSetSelfDeaf |
	messages.UserStateSetPluginContext | messages.UserStateSetPluginIdentity |
	messages.UserStateSetRecording

// adminVoiceUserStateFields are UserState fields gated behind MuteDeafen. They are
// checked as a group before any of them is applied, so a partially-authorised message
// changes nothing at all.
const adminVoiceUserStateFields = messages.UserStateSetMute | messages.UserStateSetDeaf |
	messages.UserStateSetSuppress | messages.UserStateSetPrioritySpeaker

// pluginUserStateFields are applied to server-side state but never set murmur's
// bBroadcast flag on their own (Messages.cpp:995-1007): they are stripped before
// any echo and a plugin-only message produces no UserState broadcast.
const pluginUserStateFields = messages.UserStateSetPluginContext | messages.UserStateSetPluginIdentity

// broadcastUserStateFields are the fields that cause a UserState broadcast after
// apply (murmur's bBroadcast). plugin_context/plugin_identity are intentionally
// excluded — see pluginUserStateFields.
const broadcastUserStateFields = (selfOnlyUserStateFields &^ pluginUserStateFields) |
	adminVoiceUserStateFields |
	messages.UserStateSetChannelID | messages.UserStateSetTexture |
	messages.UserStateSetComment

// handledUserStateFields are the UserState bits this server applies. A message with
// none of these set is a no-op. Broadcasting is gated separately on
// broadcastUserStateFields after plugin fields are stripped.
const handledUserStateFields = broadcastUserStateFields | pluginUserStateFields

// handleUserState implements murmur's Server::msgUserState (Messages.cpp:764-1229).
// The broadcast is a delta echo of the (possibly server-mutated) inbound message,
// never a fresh snapshot: presence on this path means "this changed", and building
// the broadcast from userToState was the v0.1.4 regression. See
// docs/architecture/protocol-encoding.md for the full delta-vs-snapshot rule.
func (s *Server) handleUserState(msgType protocol.MessageType, payload []byte, c Peer) error {
	if !s.peerActive(c) {
		return nil
	}
	var us messages.UserState
	if err := us.Unmarshal(payload); err != nil {
		return err
	}

	sender, ok := s.users.Snapshot(c.SessionID())
	if !ok {
		return nil
	}
	// Target: self (Session absent or same as sender) vs another user (admin ops)
	targetSession := us.Session
	if !us.Has(messages.UserStateSetSession) || targetSession == c.SessionID() {
		targetSession = c.SessionID()
	}
	target, ok := s.users.Snapshot(targetSession)
	if !ok {
		return nil
	}
	isAdminOp := targetSession != c.SessionID()

	deny := func(channelID uint32, denyType messages.DenyType, reason string) {
		_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{
			ChannelID: channelID, Type: denyType, Reason: reason,
		})
	}

	// Nobody but SuperUser may act on SuperUser, and this outranks every other
	// check in the handler (Messages.cpp:775-780).
	if target.IsSuperUser && !sender.IsSuperUser {
		deny(target.ChannelID, messages.DenySuperUser, "Cannot modify SuperUser")
		return nil
	}

	// Renaming is never done via UserState — names are set at Authenticate time —
	// so has_name() is an unconditional deny (Messages.cpp:784-787), independent of
	// who the message targets.
	if us.Has(messages.UserStateSetName) {
		_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{Type: messages.DenyUserName})
		return nil
	}

	// Murmur rate-limits self-targeted UserState (Messages.cpp:790) because every
	// accepted message fans out to every connected client.
	if !isAdminOp && !s.checkUserStateRateLimit(c.SessionID()) {
		return nil
	}

	// Self-registration and temporary access tokens are not implemented by this
	// server, so strip them: applying them silently would misrepresent what
	// happened, and echoing them unstripped (change 2 below) would announce a
	// state we never actually established.
	us.UserID = 0
	us.SetFields &^= messages.UserStateSetUserID
	us.TemporaryAccessTokens = nil

	// Listening fields are repeated and carry no presence bits, so a listening-only
	// message would be dropped by the has-bit gates below (SetFields==0 and the
	// handled-fields check). Pull them aside first and run them through their own
	// path (Mumble 1.4 channel listening, listeners.go).
	listeningAdd, listeningRemove, listeningVols :=
		us.ListeningChannelAdd, us.ListeningChannelRemove, us.ListeningVolumeAdjustment
	us.ListeningChannelAdd, us.ListeningChannelRemove, us.ListeningVolumeAdjustment = nil, nil, nil
	hasListening := len(listeningAdd) > 0 || len(listeningRemove) > 0 || len(listeningVols) > 0

	// Listening is a self-only preference; like murmur, a message aiming it at
	// another session is dropped silently rather than partially applied.
	if hasListening && isAdminOp {
		return nil
	}
	if hasListening {
		s.applyListening(c, target, listeningAdd, listeningRemove, listeningVols)
	}

	if us.SetFields == 0 {
		return nil
	}

	// These fields describe a client's own preferences and are meaningless when aimed
	// at somebody else. Drop the message rather than applying the remainder of it.
	if isAdminOp && us.SetFields&selfOnlyUserStateFields != 0 {
		return nil
	}

	// Permissions below are resolved against the target's current channel, before any
	// move requested by this same message is applied.
	//
	// Administrative mute/deaf/suppress/priority_speaker always require MuteDeafen,
	// including when a client targets itself — matching murmur. Clients use
	// self_mute/self_deaf for their own mute state.
	if us.SetFields&adminVoiceUserStateFields != 0 {
		// Suppress is derived from channel speak ACLs by the server, so a client may
		// only ever clear it, never assert it.
		if us.Suppress || !s.aclCheck(acl.SubjectOf(sender), target.ChannelID, mumble.PermissionMuteDeafen) {
			deny(target.ChannelID, messages.DenyPermission, "No mute/deafen permission")
			return nil
		}
		// Muting somebody inside a temporary channel is an escalation risk: anyone
		// can make a temp channel and grant themselves MuteDeafen in it. Murmur
		// therefore re-checks MuteDeafen in the first non-temporary ancestor
		// (Messages.cpp:815-833). priority_speaker is exempt upstream.
		if us.SetFields&(messages.UserStateSetMute|messages.UserStateSetDeaf|messages.UserStateSetSuppress) != 0 {
			if permanent, ok := s.firstPermanentChannel(target.ChannelID); !ok ||
				!s.aclCheck(acl.SubjectOf(sender), permanent, mumble.PermissionMuteDeafen) {
				deny(target.ChannelID, messages.DenyTemporaryChannel, "No mute/deafen permission outside temporary channel")
				return nil
			}
		}
	}

	// Comments and textures belong to their owner. Murmur lets an admin holding
	// ResetUserContent on root clear somebody else's, but never set one
	// (Messages.cpp:890-925), and bounds both against the advertised limits.
	if us.Has(messages.UserStateSetComment) {
		if isAdminOp {
			if !s.aclCheck(acl.SubjectOf(sender), s.chans.RootID(), mumble.PermissionResetUser) {
				deny(s.chans.RootID(), messages.DenyPermission, "No permission to reset user content")
				return nil
			}
			if us.Comment != "" {
				deny(target.ChannelID, messages.DenyTextTooLong, "May only clear another user's comment")
				return nil
			}
		}
		if max := s.maxTextLength(); max > 0 && len(us.Comment) > max {
			deny(target.ChannelID, messages.DenyTextTooLong, "Comment too long")
			return nil
		}
	}
	if us.Has(messages.UserStateSetTexture) {
		if max := s.maxImageLength(); max > 0 && len(us.Texture) > max {
			deny(target.ChannelID, messages.DenyTextTooLong, "Texture too large")
			return nil
		}
		if isAdminOp {
			if !s.aclCheck(acl.SubjectOf(sender), s.chans.RootID(), mumble.PermissionResetUser) {
				deny(s.chans.RootID(), messages.DenyPermission, "No permission to reset user content")
				return nil
			}
			if len(us.Texture) > 0 {
				deny(target.ChannelID, messages.DenyTextTooLong, "May only clear another user's texture")
				return nil
			}
		}
	}

	// A client that starts recording on a server which forbids it is disconnected,
	// not merely ignored (Messages.cpp:1057-1070).
	if us.Has(messages.UserStateSetRecording) && us.Recording && !target.Recording && !s.recordingAllowed() {
		s.sendThenClose(cluster.SessionRef{SessionID: targetSession, Generation: target.SessionGeneration}, protocol.MessageUserRemove, &messages.UserRemove{
			Session: targetSession,
			Reason:  "Recording is not allowed on this server",
		})
		return nil
	}

	// recording only counts as a change (and only ever broadcasts) when the value
	// actually differs from the stored state (Messages.cpp:1048); resending the
	// current value is a no-op even though the field is present on the wire.
	if us.Has(messages.UserStateSetRecording) && us.Recording == target.Recording {
		us.SetFields &^= messages.UserStateSetRecording
	}

	if us.SetFields&handledUserStateFields == 0 {
		return nil
	}

	channelMoved := false
	var maySpeakAtDest bool
	if us.Has(messages.UserStateSetChannelID) && s.chans != nil {
		if _, exists := s.chans.GetChannel(us.ChannelID); !exists {
			return nil
		}
		if us.ChannelID == target.ChannelID {
			// Requesting the channel you're already in drops the entire message, not
			// just the move (Messages.cpp:800-803) — matching murmur exactly.
			return nil
		}
		// Two independent checks, both from Messages.cpp:805-813. Moving somebody
		// else additionally requires Move where they currently are; every move then
		// requires either Move on the destination (an admin pulling someone in) or
		// the target's own right to Enter it.
		if isAdminOp && !s.aclCheck(acl.SubjectOf(sender), target.ChannelID, mumble.PermissionMove) {
			deny(target.ChannelID, messages.DenyPermission, "No move permission")
			return nil
		}
		if !s.aclCheck(acl.SubjectOf(sender), us.ChannelID, mumble.PermissionMove) &&
			!s.aclCheck(acl.SubjectOf(target), us.ChannelID, mumble.PermissionEnter) {
			deny(us.ChannelID, messages.DenyPermission, "Permission denied")
			return nil
		}
		if s.channelIsFull(us.ChannelID, acl.SubjectOf(sender)) {
			deny(us.ChannelID, messages.DenyChannelFull, "Channel is full")
			return nil
		}
		channelMoved = true
		oldChannelID := target.ChannelID
		s.users.SetChannel(targetSession, us.ChannelID)
		target.ChannelID = us.ChannelID
		s.invalidateACLCache()
		s.UpdateChannelCrypto(oldChannelID)
		s.UpdateChannelCrypto(us.ChannelID)
		if targetSession == c.SessionID() {
			// Refresh the mover's own permission cache through the control
			// transport FIFO, so remote sessions get it too (Messages.cpp:842).
			perms := s.aclPermissions(acl.SubjectOf(target), us.ChannelID)
			s.sendControl(targetSession, protocol.MessagePermissionQuery, &messages.PermissionQuery{
				ChannelID:   us.ChannelID,
				Permissions: uint32(perms),
			})
		}
		// Resolved here, before UpdateUser — see maySpeak.
		maySpeakAtDest = s.maySpeak(acl.SubjectOf(target), us.ChannelID)
	}

	var priorityChanged, suppressChanged bool
	updated, ok := s.users.UpdateUser(targetSession, func(u *mumble.User) {
		applySelfVoiceState(&u.VoiceState, &us)
		applyAdminVoiceState(&u.VoiceState, &us)
		if channelMoved {
			priorityChanged, suppressChanged = applyChannelEnterEffects(u, maySpeakAtDest)
		}
		// Presence, not emptiness, decides here: an empty texture or comment is the
		// admin "reset this user's content" operation and has to actually clear it.
		if us.Has(messages.UserStateSetTexture) {
			u.Texture = append([]byte(nil), us.Texture...)
		}
		if us.Has(messages.UserStateSetComment) {
			u.Comment = us.Comment
		}
		if us.Has(messages.UserStateSetPluginIdentity) {
			u.PluginIdentity = us.PluginIdentity
		}
		if us.Has(messages.UserStateSetPluginContext) {
			u.PluginContext = append([]byte(nil), us.PluginContext...)
		}
	})
	if !ok {
		return nil
	}

	// Suppress/priority-speaker recomputation from a channel move is echoed only
	// when it actually flipped (Server.cpp:2042-2055); if the client also asserted
	// one of these itself in the same message, applyAdminVoiceState above already
	// set the corresponding field, so this only ever adds what changed.
	if priorityChanged {
		us.PrioritySpeaker = false
		us.SetFields |= messages.UserStateSetPrioritySpeaker
	}
	if suppressChanged {
		us.Suppress = updated.Suppress
		us.SetFields |= messages.UserStateSetSuppress
	}

	// plugin_context/plugin_identity are applied to server state above but never
	// transmitted to clients (Mumble.proto), and never set murmur's bBroadcast on
	// their own (Messages.cpp:995-1007). Strip them before the broadcast gate.
	us.PluginContext = nil
	us.PluginIdentity = ""
	us.SetFields &^= pluginUserStateFields

	// A plugin-only (or otherwise non-broadcast) update must not emit a UserState
	// with just session/actor — that would be a presence-only no-op spam.
	if us.SetFields&broadcastUserStateFields == 0 {
		return nil
	}

	us.Session = targetSession
	us.Actor = c.SessionID()
	us.SetFields |= messages.UserStateSetSession | messages.UserStateSetActor

	s.Broadcast(0, protocol.MessageUserState, &us)
	if channelMoved {
		// Preserve Murmur's PermissionQuery/UserState ordering, then refresh the
		// moved user's recipient-specific lock state for @in/@out/@sub rules.
		s.RefreshEnterStatesFor(targetSession)
	}

	if us.Has(messages.UserStateSetRecording) {
		s.broadcastRecordingAnnouncement(updated.Name, updated.Recording)
	}
	return nil
}

// version1_2_3 is Mumble 1.2.3 packed the same way Version.version_v2 packs
// major/minor/patch (research/mumble/src/Version.h: fromComponents).
const version1_2_3 = uint64(1)<<48 | uint64(2)<<32 | uint64(3)<<16

// broadcastRecordingAnnouncement sends the legacy "started/stopped recording"
// TextMessage only to clients older than 1.2.3 (Messages.cpp:1075); modern clients
// render their own line from the recording field on the UserState broadcast, so
// sending this to everyone would duplicate it.
func (s *Server) broadcastRecordingAnnouncement(name string, recording bool) {
	var text string
	if recording {
		text = fmt.Sprintf("User '%s' started recording", name)
	} else {
		text = fmt.Sprintf("User '%s' stopped recording", name)
	}
	tm := &messages.TextMessage{TreeID: []uint32{0}, Message: text}
	for _, snap := range s.registry.Sessions() {
		if !deliverableState(snap.State) {
			continue
		}
		// Version is Core-owned user metadata, so remote sessions without a
		// local conn are filtered identically.
		u, ok := s.users.Snapshot(snap.Ref.SessionID)
		if !ok || u.SessionGeneration != snap.Ref.Generation || u.ClientVersion >= version1_2_3 {
			continue
		}
		_ = s.control.Send(snap.Ref, cluster.ControlMessage{Type: protocol.MessageTextMessage, Message: tm})
	}
}

// handleCryptSetup is a Local Edge entry: nonce resync reads the UDP crypto
// state, which never crosses the Core/Edge boundary. The resync itself is the
// shared edge.CryptSetupResync so a Remote Edge behaves identically.
func (s *Server) handleCryptSetup(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
	return edge.CryptSetupResync(ctx.(*connection.Conn), payload)
}

func (s *Server) handleChannelState(msgType protocol.MessageType, payload []byte, c Peer) error {
	if !s.peerActive(c) {
		return nil
	}
	var cs messages.ChannelState
	if err := cs.Unmarshal(payload); err != nil {
		return err
	}
	u, ok := s.users.Snapshot(c.SessionID())
	if !ok {
		return nil
	}
	if cs.ChannelID == 0 {
		parent := cs.Parent
		if parent == 0 {
			parent = s.chans.RootID()
		}
		if !s.aclCheck(acl.SubjectOf(u), parent, mumble.PermissionMakeChannel) {
			_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{ChannelID: parent, Type: messages.DenyPermission, Reason: "Cannot create channel"})
			return nil
		}
		ch := s.chans.Create(parent, cs.Name, cs.Description, cs.Position, cs.Temporary, cs.MaxUsers)
		if ch == nil {
			_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{Type: messages.DenyPermission, Reason: "Cannot create channel"})
			return nil
		}
		s.BroadcastChannelState(ch)
		return nil
	}
	opts := channel.UpdateOpts{}
	hasMetadataChange := false
	if cs.Name != "" {
		opts.Name = &cs.Name
		hasMetadataChange = true
	}
	if cs.Description != "" {
		opts.Description = &cs.Description
		hasMetadataChange = true
	}
	if cs.Position != 0 {
		opts.Position = &cs.Position
		hasMetadataChange = true
	}
	if cs.MaxUsers != 0 {
		opts.MaxUsers = &cs.MaxUsers
		hasMetadataChange = true
	}
	if cs.Temporary {
		opts.Temporary = &cs.Temporary
		hasMetadataChange = true
	}

	ch, exists := s.chans.GetChannel(cs.ChannelID)
	if !exists {
		return nil
	}
	desiredLinks := make(map[uint32]bool, len(ch.Links))
	for _, id := range ch.Links {
		desiredLinks[id] = true
	}
	originalLinks := make(map[uint32]bool, len(desiredLinks))
	for id := range desiredLinks {
		originalLinks[id] = true
	}
	hasLinkChange := len(cs.Links) > 0 || len(cs.LinksAdd) > 0 || len(cs.LinksRemove) > 0
	if len(cs.Links) > 0 {
		desiredLinks = make(map[uint32]bool, len(cs.Links))
		for _, id := range cs.Links {
			desiredLinks[id] = true
		}
	}
	for _, id := range cs.LinksAdd {
		desiredLinks[id] = true
	}
	for _, id := range cs.LinksRemove {
		delete(desiredLinks, id)
	}
	if hasMetadataChange && !s.aclCheck(acl.SubjectOf(u), cs.ChannelID, mumble.PermissionWrite) {
		_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{ChannelID: cs.ChannelID, Type: messages.DenyPermission})
		return nil
	}
	if hasLinkChange {
		for id := range desiredLinks {
			if originalLinks[id] {
				continue
			}
			if id == cs.ChannelID || !s.aclCheck(acl.SubjectOf(u), cs.ChannelID, mumble.PermissionLinkChannel) ||
				!s.aclCheck(acl.SubjectOf(u), id, mumble.PermissionLinkChannel) {
				_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{ChannelID: cs.ChannelID, Type: messages.DenyPermission})
				return nil
			}
		}
		for _, id := range ch.Links {
			if desiredLinks[id] {
				continue
			}
			if !s.aclCheck(acl.SubjectOf(u), cs.ChannelID, mumble.PermissionLinkChannel) &&
				!s.aclCheck(acl.SubjectOf(u), id, mumble.PermissionLinkChannel) {
				_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{ChannelID: cs.ChannelID, Type: messages.DenyPermission})
				return nil
			}
		}
	}
	if hasMetadataChange && s.chans.Update(cs.ChannelID, opts) {
		if updated, ok := s.chans.GetChannel(cs.ChannelID); ok {
			s.BroadcastChannelState(updated)
		}
	}
	if hasLinkChange {
		desired := make([]uint32, 0, len(desiredLinks))
		for id := range desiredLinks {
			desired = append(desired, id)
		}
		changed, updated := s.chans.UpdateLinks(cs.ChannelID, desired)
		if !updated {
			_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{ChannelID: cs.ChannelID, Type: messages.DenyPermission})
			return nil
		}
		for _, id := range changed {
			if linked, ok := s.chans.GetChannel(id); ok {
				s.BroadcastChannelState(linked)
			}
		}
	}
	return nil
}

func (s *Server) handleChannelRemove(msgType protocol.MessageType, payload []byte, c Peer) error {
	if !s.peerActive(c) {
		return nil
	}
	var cr messages.ChannelRemove
	if err := cr.Unmarshal(payload); err != nil {
		return err
	}
	u, ok := s.users.Snapshot(c.SessionID())
	if !ok {
		return nil
	}
	if !s.aclCheck(acl.SubjectOf(u), cr.ChannelID, mumble.PermissionWrite) {
		_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{ChannelID: cr.ChannelID, Type: messages.DenyPermission})
		return nil
	}
	ch, ok := s.chans.GetChannel(cr.ChannelID)
	if !ok {
		return nil
	}
	parentID := ch.ParentID
	if parentID == 0 {
		parentID = s.chans.RootID()
	}
	for _, mu := range s.users.SnapshotByChannel(cr.ChannelID) {
		sid := mu.SessionID
		// Resolved before UpdateUser — see maySpeak.
		maySpeak := s.maySpeak(acl.SubjectOf(mu), parentID)
		s.users.SetChannel(sid, parentID)
		var priorityChanged, suppressChanged bool
		updated, ok := s.users.UpdateUser(sid, func(u *mumble.User) {
			priorityChanged, suppressChanged = applyChannelEnterEffects(u, maySpeak)
		})
		if !ok {
			continue
		}
		// Minimal {session, channel_id} delta per moved user, plus suppress/priority
		// speaker only when userEnterChannel actually flipped them, matching murmur
		// rather than broadcasting a full snapshot.
		state := &messages.UserState{
			Session:   sid,
			ChannelID: parentID,
			SetFields: messages.UserStateSetSession | messages.UserStateSetChannelID,
		}
		if priorityChanged {
			state.SetFields |= messages.UserStateSetPrioritySpeaker
		}
		if suppressChanged {
			state.Suppress = updated.Suppress
			state.SetFields |= messages.UserStateSetSuppress
		}
		s.Broadcast(0, protocol.MessageUserState, state)
	}
	// Listeners learn the channel is gone via a listening_channel_remove delta that
	// precedes the ChannelRemove broadcast, mirroring murmur's ordering.
	for _, sid := range s.listeners.RemoveChannel(cr.ChannelID) {
		s.Broadcast(0, protocol.MessageUserState, &messages.UserState{
			Session:                sid,
			SetFields:              messages.UserStateSetSession,
			ListeningChannelRemove: []uint32{cr.ChannelID},
		})
	}
	// The channel these users were in is going away, and they have all moved.
	s.invalidateACLCache()
	if s.chans.Remove(cr.ChannelID) {
		s.Broadcast(0, protocol.MessageChannelRemove, &cr)
	}
	s.UpdateChannelCrypto(parentID)
	return nil
}

func (s *Server) handleTextMessage(msgType protocol.MessageType, payload []byte, c Peer) error {
	if !s.peerActive(c) {
		return nil
	}
	var tm messages.TextMessage
	if err := tm.Unmarshal(payload); err != nil {
		return err
	}
	u, ok := s.users.Snapshot(c.SessionID())
	if !ok {
		return nil
	}
	if !s.checkTextRateLimit(c.SessionID()) {
		_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{Type: messages.DenyTextTooLong, Reason: "Rate limit exceeded"})
		return nil
	}
	// Murmur's isTextAllowed: reject anything over the advertised message_length
	// rather than relaying it (Messages.cpp, Server::isTextAllowed).
	if max := s.maxTextLength(); max > 0 && len(tm.Message) > max {
		_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{Type: messages.DenyTextTooLong})
		return nil
	}
	hasTarget := len(tm.Session) > 0 || len(tm.ChannelID) > 0 || len(tm.TreeID) > 0
	if hasTarget {
		for _, sid := range tm.Session {
			targetUser, ok := s.users.Snapshot(sid)
			if !ok {
				continue
			}
			if !s.aclCheck(acl.SubjectOf(u), targetUser.ChannelID, mumble.PermissionTextMessage) {
				_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{
					ChannelID: targetUser.ChannelID, Type: messages.DenyPermission, Reason: "No text permission",
				})
				return nil
			}
		}
		for _, cid := range tm.ChannelID {
			if !s.aclCheck(acl.SubjectOf(u), cid, mumble.PermissionTextMessage) {
				_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{ChannelID: cid, Type: messages.DenyPermission, Reason: "No text permission"})
				return nil
			}
		}
		for _, tid := range tm.TreeID {
			if !s.aclCheck(acl.SubjectOf(u), tid, mumble.PermissionTextMessage) {
				_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{ChannelID: tid, Type: messages.DenyPermission, Reason: "No text permission"})
				return nil
			}
		}
	} else if !s.aclCheck(acl.SubjectOf(u), u.ChannelID, mumble.PermissionTextMessage) {
		_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{ChannelID: u.ChannelID, Type: messages.DenyPermission, Reason: "No text permission"})
		return nil
	}
	tm.Actor = c.SessionID()
	var recipients []uint32
	if len(tm.Session) > 0 {
		recipients = append(recipients, tm.Session...)
	}
	if len(tm.ChannelID) > 0 {
		for _, cid := range tm.ChannelID {
			recipients = append(recipients, s.users.SessionIDsInChannel(cid)...)
		}
	}
	if len(tm.TreeID) > 0 {
		for _, tid := range tm.TreeID {
			for _, cid := range s.chans.SubtreeIDs(tid) {
				if !s.aclCheck(acl.SubjectOf(u), cid, mumble.PermissionTextMessage) {
					continue
				}
				recipients = append(recipients, s.users.SessionIDsInChannel(cid)...)
			}
		}
	}
	senderSession := c.SessionID()
	seen := make(map[uint32]bool)
	for _, sid := range recipients {
		if sid == senderSession || seen[sid] {
			continue
		}
		seen[sid] = true
		s.sendControl(sid, protocol.MessageTextMessage, &tm)
	}
	return nil
}

func (s *Server) checkTextRateLimit(sessionID uint32) bool {
	return rateLimitAllows(&s.textRateLimiter, sessionID, maxTextMessagesPerSecond)
}

// checkUserStateRateLimit throttles self-targeted UserState (murmur's RATELIMIT at
// Messages.cpp:790). The budget is well above what a human toggling mute can
// produce, so it only bites on a client stuck in an echo loop.
func (s *Server) checkUserStateRateLimit(sessionID uint32) bool {
	return rateLimitAllows(&s.userStateRateLimiter, sessionID, maxUserStatesPerSecond)
}

const (
	maxTextMessagesPerSecond = 30
	maxUserStatesPerSecond   = 10
)

func (s *Server) conn(sessionID uint32) *connection.Conn {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	return s.conns[sessionID]
}

func (s *Server) handleVoiceTarget(msgType protocol.MessageType, payload []byte, c Peer) error {
	if !s.peerActive(c) {
		return nil
	}
	var vt messages.VoiceTarget
	if err := vt.Unmarshal(payload); err != nil {
		return err
	}
	if vt.ID == 0 || vt.ID > 30 {
		return nil
	}
	s.storeVoiceTarget(c, vt)
	return nil
}

var udpTunnelCount sync.Map // session -> *uint64

// handleUDPTunnel is a Local Edge entry: TCP-tunneled audio is decoded with
// the connection's negotiated wire mode and fed into the Core voice router
// as a canonical frame.
func (s *Server) handleUDPTunnel(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
	c := ctx.(*connection.Conn)
	if c.State() != connection.StateActive || s.router == nil {
		return nil
	}
	frame, err := s.decodeAudio(c, payload, "tcp")
	if err != nil {
		return err
	}
	return s.router.RouteRef(cluster.SessionRef{SessionID: c.SessionID(), Generation: c.SessionGeneration()}, frame)
}

func (s *Server) handleBanList(msgType protocol.MessageType, payload []byte, c Peer) error {
	if !s.peerActive(c) {
		return nil
	}
	var bl messages.BanList
	if len(payload) > 0 {
		_ = bl.Unmarshal(payload)
	}
	u, ok := s.users.Snapshot(c.SessionID())
	if !ok {
		return nil
	}
	if !s.aclCheck(acl.SubjectOf(u), s.chans.RootID(), mumble.PermissionBan) {
		_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{Type: messages.DenyPermission, Reason: "No ban permission"})
		return nil
	}
	if !bl.Query {
		if err := s.bans.Replace(bl.Bans); err != nil {
			slog.Debug("ban list replace failed", "err", err)
			return nil
		}
	}
	bl.Bans = s.bans.List()
	bl.Query = false
	_ = c.WriteMessage(protocol.MessageBanList, &bl)
	return nil
}

func (s *Server) handleACL(msgType protocol.MessageType, payload []byte, c Peer) error {
	if !s.peerActive(c) {
		return nil
	}
	var aclMsg messages.ACL
	if len(payload) > 0 {
		_ = aclMsg.Unmarshal(payload)
	}
	if aclMsg.Query {
		aclMsg.Query = false
		aclMsg.InheritACLs = true
		_ = c.WriteMessage(protocol.MessageACL, &aclMsg)
	}
	return nil
}

func (s *Server) handlePermissionQuery(msgType protocol.MessageType, payload []byte, c Peer) error {
	if !s.peerActive(c) {
		return nil
	}
	var pq messages.PermissionQuery
	if len(payload) > 0 {
		_ = pq.Unmarshal(payload)
	}
	u, ok := s.users.Snapshot(c.SessionID())
	if !ok {
		return nil
	}
	// ChannelID 0 = root channel (per Mumble protocol)
	cid := pq.ChannelID
	pq.Permissions = uint32(s.aclPermissions(acl.SubjectOf(u), cid))
	_ = c.WriteMessage(protocol.MessagePermissionQuery, &pq)
	return nil
}

func (s *Server) handleRequestBlob(msgType protocol.MessageType, payload []byte, c Peer) error {
	if !s.peerActive(c) {
		return nil
	}
	var rb messages.RequestBlob
	if len(payload) > 0 {
		_ = rb.Unmarshal(payload)
	}
	s.sendRequestedBlobs(c, &rb)
	return nil
}

func (s *Server) sendRequestedBlobs(c Peer, rb *messages.RequestBlob) {
	for _, sid := range rb.SessionTexture {
		if u, ok := s.users.Snapshot(sid); ok && len(u.Texture) > 0 {
			_ = c.WriteMessage(protocol.MessageUserState, &messages.UserState{
				Session:   sid,
				Texture:   u.Texture,
				SetFields: messages.UserStateSetSession | messages.UserStateSetTexture,
			})
		}
	}
	for _, sid := range rb.SessionComment {
		if u, ok := s.users.Snapshot(sid); ok && u.Comment != "" {
			_ = c.WriteMessage(protocol.MessageUserState, &messages.UserState{
				Session:   sid,
				Comment:   u.Comment,
				SetFields: messages.UserStateSetSession | messages.UserStateSetComment,
			})
		}
	}
	for _, cid := range rb.ChannelDescription {
		if ch, ok := s.chans.GetChannel(cid); ok && ch.Description != "" {
			_ = c.WriteMessage(protocol.MessageChannelState, &messages.ChannelState{ChannelID: cid, Description: ch.Description})
		}
	}
}

func (s *Server) handleUserStats(msgType protocol.MessageType, payload []byte, c Peer) error {
	if !s.peerActive(c) {
		return nil
	}
	var req messages.UserStats
	if len(payload) > 0 {
		_ = req.Unmarshal(payload)
	}
	if req.Session == 0 {
		req.Session = c.SessionID()
	}
	if s.users.Exists(req.Session) {
		_ = c.WriteMessage(protocol.MessageUserStats, &req)
	}
	return nil
}

func (s *Server) handleQueryUsers(msgType protocol.MessageType, payload []byte, c Peer) error {
	if !s.peerActive(c) {
		return nil
	}
	var qu messages.QueryUsers
	if len(payload) > 0 {
		_ = qu.Unmarshal(payload)
	}
	resp := messages.QueryUsers{}
	resolved, err := s.authority.Resolve(context.Background(), identity.ResolveRequest{UserIDs: qu.IDs, Names: qu.Names})
	if err != nil {
		resp.Names = make([]string, len(qu.IDs))
		resp.IDs = make([]uint32, len(qu.Names))
		_ = c.WriteMessage(protocol.MessageQueryUsers, &resp)
		return nil
	}
	nameByID := make(map[uint32]string, len(resolved))
	idByName := make(map[string]uint32, len(resolved))
	for _, item := range resolved {
		if item.Eligible {
			nameByID[item.UserID] = item.Name
			idByName[item.Name] = item.UserID
		}
	}
	for _, userID := range qu.IDs {
		resp.Names = append(resp.Names, nameByID[userID])
	}
	for _, name := range qu.Names {
		resp.IDs = append(resp.IDs, idByName[name])
	}
	_ = c.WriteMessage(protocol.MessageQueryUsers, &resp)
	return nil
}

func (s *Server) handleUserList(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
	_ = ctx
	_ = payload
	return nil
}

func (s *Server) handleContextActionModify(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
	_ = ctx
	_ = payload
	return nil
}

func (s *Server) handleContextAction(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
	_ = ctx
	_ = payload
	return nil
}

func (s *Server) handlePluginDataTransmission(msgType protocol.MessageType, payload []byte, ctx interface{}) error {
	_ = ctx
	_ = payload
	return nil
}

// ProtocolVersionV1/V2 返回服务端当前正式宣告的协议能力。
func (s *Server) ProtocolVersionV1() uint32 {
	return ServerVersionV1
}
func (s *Server) ProtocolVersionV2() uint64 {
	return ServerVersionV2
}

var audioParseCount atomic.Uint64
var audioParseFailures atomic.Uint64

func (s *Server) decodeAudio(c *connection.Conn, payload []byte, transport string) (mumbleaudio.Frame, error) {
	frame, err := mumbleaudio.DecodeClientPacket(c.AudioWireMode(), payload)
	if s.voiceDebug.Load() {
		count := audioParseCount.Add(1)
		if err != nil {
			failed := audioParseFailures.Add(1)
			if failed <= 5 || failed%50 == 0 {
				slog.Warn("audio parse failed", "session", c.SessionID(), "wire_mode", c.AudioWireMode(), "transport", transport, "count", failed, "error", err)
			}
		} else if count <= 5 || count%50 == 0 {
			slog.Info("audio received", "session", c.SessionID(), "wire_mode", c.AudioWireMode(), "transport", transport, "target", frame.Target, "count", count)
		}
	}
	return frame, err
}
