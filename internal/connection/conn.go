package connection

import (
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"github.com/dchote/go-mumble-server/pkg/mumble/crypto"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// State is the connection state.
type State int

const (
	StateAuthenticating State = iota
	StateActive
	StateClosed
)

// Conn represents a Mumble client connection.
type Conn struct {
	net.Conn
	Crypt *crypto.CryptState

	mu                sync.RWMutex
	state             State
	writing           bool
	sessionID         uint32
	sessionGeneration uint64 // logical session generation; with sessionID it identifies the non-reusable session for teardown
	clientCryptoModes uint32 // bitmask from Version.CryptoModes; 0 = legacy only (standard client)
	audioWireMode     audio.WireMode
	clientVersion     uint64 // packed major<<48|minor<<32|patch<<16 from Version; 0 = unknown, treated as "very old"
	serverID          uint
	userName          string

	writeCh chan writeReq
	pending atomic.Int64
	done    chan struct{}
	onClose func(*Conn)
	// pingMetric reports the client-advertised TCP latency (typed business
	// metric) up to Core. Ping itself terminates at the edge.
	pingMetric func(*Conn, float32)
}

// flushTimeout bounds how long a kick or ban waits for the queued reason to
// reach the socket before the connection is torn down anyway.
const flushTimeout = 250 * time.Millisecond

type writeReq struct {
	msgType protocol.MessageType
	msg     messages.Message
	payload []byte
}

// New creates a new connection.
func New(c net.Conn, crypt *crypto.CryptState, onClose func(*Conn)) *Conn {
	return &Conn{
		Conn:    c,
		Crypt:   crypt,
		state:   StateAuthenticating,
		writeCh: make(chan writeReq, 64),
		done:    make(chan struct{}),
		onClose: onClose,
	}
}

// SessionID returns the session ID (0 until authenticated).
func (c *Conn) SessionID() uint32 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionID
}

// SetSessionID sets the session ID.
func (c *Conn) SetSessionID(id uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionID = id
}

// SetSessionGeneration records the bound session's generation. It is set at
// authentication time and never changes afterwards, so a delayed disconnect
// callback can still identify the exact logical session it belonged to.
func (c *Conn) SetSessionGeneration(g uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionGeneration = g
}

// SessionGeneration returns the bound session's generation (0 until authenticated).
func (c *Conn) SessionGeneration() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionGeneration
}

// State returns the connection state.
func (c *Conn) State() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// SetActive sets state to Active.
func (c *Conn) SetActive() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateActive
}

// SetUserName records the authenticated user's name for logging.
func (c *Conn) SetUserName(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userName = name
}

// CertificateHash returns the SHA-1 hex fingerprint of the client's TLS certificate, or empty if unavailable.
func (c *Conn) CertificateHash() string {
	tlsConn, ok := c.Conn.(*tls.Conn)
	if !ok {
		return ""
	}
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return ""
	}
	der := state.PeerCertificates[0].Raw
	hash := sha1.Sum(der)
	return strings.ToLower(hex.EncodeToString(hash[:]))
}

// UserName returns the username, empty until the client authenticates.
func (c *Conn) UserName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.userName
}

// ClientCryptoModes returns the client's advertised crypto mode bitmask (0 = legacy only).
func (c *Conn) ClientCryptoModes() uint32 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.clientCryptoModes
}

// SetClientCryptoModes stores the client's advertised crypto mode bitmask.
func (c *Conn) SetClientCryptoModes(m uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clientCryptoModes = m
}

// ClientVersion returns the client's packed protocol version (major<<48|minor<<32|patch<<16),
// or 0 if the client has not sent a Version message yet (treated as "very old").
func (c *Conn) ClientVersion() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.clientVersion
}

// SetClientVersion stores the client's packed protocol version.
func (c *Conn) SetClientVersion(v uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clientVersion = v
}

// WriteMessage queues a message for sending.
func (c *Conn) WriteMessage(msgType protocol.MessageType, msg messages.Message) error {
	return c.enqueue(writeReq{msgType: msgType, msg: messages.Clone(msg)})
}

// WriteRaw queues raw payload for sending.
func (c *Conn) WriteRaw(msgType protocol.MessageType, payload []byte) error {
	return c.enqueue(writeReq{msgType: msgType, payload: append([]byte(nil), payload...)})
}

func (c *Conn) enqueue(req writeReq) error {
	select {
	case <-c.done:
		return net.ErrClosed
	default:
	}
	// Increment before the send: a flush that races the enqueue must see the
	// message as pending, otherwise it could close the socket while the
	// message (e.g. a Reject) is still queued and drop it.
	c.pending.Add(1)
	select {
	case c.writeCh <- req:
		return nil
	default:
		c.pending.Add(-1)
		return io.ErrShortWrite
	}
}

// Close closes the connection and notifies onClose.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.state == StateClosed {
		c.mu.Unlock()
		return nil
	}
	c.state = StateClosed
	onClose := c.onClose
	c.mu.Unlock()
	close(c.done)
	err := c.Conn.Close()
	if onClose != nil {
		onClose(c)
	}
	return err
}

// CloseAfterFlush waits for queued messages to reach the socket before closing,
// so a kick or ban reason is actually delivered. Mirrors Murmur's forceFlush()
// followed by disconnectSocket(). The wait is bounded by flushTimeout. pending
// alone gates the wait: a message enqueued before the write loop started is
// still pending and must not be closed away.
func (c *Conn) CloseAfterFlush() error {
	c.mu.RLock()
	closed := c.state == StateClosed
	c.mu.RUnlock()
	if !closed {
		deadline := time.Now().Add(flushTimeout)
		for c.pending.Load() > 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	return c.Close()
}

// AwaitFlush is the write receipt: it returns once every queued message has
// reached the socket, or when ctx expires first. The wait is additionally
// bounded by flushTimeout so a stalled client cannot pin the caller.
func (c *Conn) AwaitFlush(ctx context.Context) error {
	ctx2, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for c.pending.Load() > 0 {
		select {
		case <-ctx2.Done():
			return ctx2.Err()
		case <-ticker.C:
		}
	}
	return nil
}

// SetPingReporter installs the Core-side sink for client-advertised TCP
// latency. Ping replies themselves never leave the edge.
func (c *Conn) SetPingReporter(fn func(*Conn, float32)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pingMetric = fn
}

// replyPing terminates TCP Ping at the edge (split-owned protocol): the echo
// timestamp and the local UDP crypto counters stay here; only TCPPingAvg is
// reported up as a typed business metric.
func (c *Conn) replyPing(payload []byte) {
	resp := messages.Ping{}
	if len(payload) > 0 {
		var p messages.Ping
		if err := p.Unmarshal(payload); err != nil {
			return
		}
		resp.Timestamp = p.Timestamp
		c.mu.RLock()
		report := c.pingMetric
		c.mu.RUnlock()
		if p.TCPPingAvg > 0 && report != nil {
			report(c, p.TCPPingAvg)
		}
	}
	if resp.Timestamp == 0 {
		resp.Timestamp = uint64(time.Now().UnixMicro())
	}
	if c.Crypt != nil {
		resp.Good = c.Crypt.Good
		resp.Late = c.Crypt.Late
		resp.Lost = c.Crypt.Lost
		resp.Resync = c.Crypt.Resync
	}
	_ = c.WriteMessage(protocol.MessagePing, &resp)
}

// Run runs the read and write loops.
func (c *Conn) Run(ctx context.Context, handler protocol.HandlerTable) error {
	defer c.Close()
	c.mu.Lock()
	c.writing = true
	c.mu.Unlock()
	go c.writeLoop()

	for {
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		msgType, payload, err := protocol.ReadPacket(c.Conn)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		c.mu.RLock()
		state := c.state
		c.mu.RUnlock()
		if state == StateAuthenticating && msgType != protocol.MessageVersion && msgType != protocol.MessageAuthenticate {
			continue
		}
		if msgType == protocol.MessagePing {
			c.replyPing(payload)
			continue
		}
		if err := handler.Dispatch(msgType, payload, c); err != nil {
			slog.Debug("handler error", "type", msgType, "err", err)
		}
	}
}

func (c *Conn) writeLoop() {
	for {
		select {
		case req := <-c.writeCh:
			c.writeOne(req)
		case <-c.done:
			return
		}
	}
}

func (c *Conn) writeOne(req writeReq) {
	defer c.pending.Add(-1)
	if len(req.payload) > 0 {
		protocol.WritePacket(c, req.msgType, req.payload)
	} else {
		protocol.WriteMessage(c, req.msgType, req.msg)
	}
}

// CertificateVerified 只信任 TLS 已验证的证书链，不以证书存在代替验证。
func (c *Conn) CertificateVerified() bool {
	conn, ok := c.Conn.(*tls.Conn)
	return ok && len(conn.ConnectionState().VerifiedChains) > 0
}

// NegotiateAudioWireMode 仅在认证前更新，UDP 与 TCP 共用已固定的协商结果。
func (c *Conn) NegotiateAudioWireMode(serverVersion uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == StateAuthenticating {
		c.audioWireMode = audio.NegotiateWireMode(c.clientVersion, serverVersion)
	}
}
func (c *Conn) AudioWireMode() audio.WireMode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.audioWireMode
}
