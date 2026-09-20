package edge

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

var ErrCoreProtocolNotImplemented = errors.New("edge-core transport is not connected")

type RemoteCore interface {
	Authenticate(context.Context, AuthForward) (AuthResult, error)
	ForwardControl(context.Context, uint32, protocol.MessageType, []byte) error
	Close() error
}

type RemoteCoreTransport interface {
	RemoteCore
	Start(context.Context) error
	SetDelivery(RemoteDelivery)
	TransportReady(context.Context, uint32, cluster.SessionRef) error
	VoiceUp(context.Context, cluster.SessionRef, audio.Frame) error
	Disconnect(cluster.SessionRef)
}

type RemoteDelivery interface {
	DeliverControl(cluster.ControlWire)
	FlushControl(cluster.SessionRef) error
	DeliverVoice(cluster.VoiceBatch)
	CloseSession(cluster.SessionRef)
	CoreDisconnected(cluster.CoreEpoch)
}

type AuthForward struct {
	ConnID              uint32
	Username            string
	Password            string
	Tokens              []string
	CertHash            string
	CertificateVerified bool
	RemoteIP            string
	ClientVersion       uint64
	ClientCryptoModes   uint32
}

type AuthResult struct {
	SessionID    uint32
	Generation   uint64
	Username     string
	Rejected     bool
	RejectType   messages.RejectType
	RejectReason string
}

type RemoteCoreClientConfig struct {
	Address           string
	EdgeID            cluster.EdgeID
	TLSConfig         *tls.Config
	ReconnectMin      time.Duration
	ReconnectMax      time.Duration
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	QueueSize         int
}

type pendingResult struct {
	response cluster.AuthResponse
	err      error
}

type RemoteCoreClient struct {
	cfg       RemoteCoreClientConfig
	mu        sync.RWMutex
	conn      net.Conn
	instance  cluster.EdgeInstanceRef
	epoch     cluster.CoreEpoch
	delivery  RemoteDelivery
	out       chan clientOut
	done      chan struct{}
	closeOnce sync.Once
	nextID    atomic.Uint64
	pendingMu sync.Mutex
	pending   map[uint64]chan pendingResult
	sessions  map[uint32]cluster.SessionRef
}

type clientOut struct {
	typ   cluster.WireType
	id    uint64
	value any
	done  chan error
}

// NewRemoteCoreClient is kept as a validating compatibility constructor.
// A TLS identity is supplied by the composition root through the config form.
func NewRemoteCoreClient(coreAddress, _ string) (*RemoteCoreClient, error) {
	return NewRemoteCoreClientWithConfig(RemoteCoreClientConfig{Address: coreAddress, EdgeID: "unconfigured"})
}

func NewRemoteCoreClientWithConfig(cfg RemoteCoreClientConfig) (*RemoteCoreClient, error) {
	if cfg.Address == "" {
		return nil, errors.New("remote core address is required")
	}
	if _, _, err := net.SplitHostPort(cfg.Address); err != nil {
		return nil, fmt.Errorf("remote core address %q is not host:port: %w", cfg.Address, err)
	}
	if cfg.ReconnectMin <= 0 {
		cfg.ReconnectMin = 250 * time.Millisecond
	}
	if cfg.ReconnectMax <= 0 {
		cfg.ReconnectMax = 5 * time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 5 * time.Second
	}
	if cfg.HeartbeatTimeout <= cfg.HeartbeatInterval {
		cfg.HeartbeatTimeout = 3 * cfg.HeartbeatInterval
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	return &RemoteCoreClient{cfg: cfg, out: make(chan clientOut, cfg.QueueSize), done: make(chan struct{}), pending: make(map[uint64]chan pendingResult), sessions: make(map[uint32]cluster.SessionRef)}, nil
}

func (c *RemoteCoreClient) Address() string              { return c.cfg.Address }
func (c *RemoteCoreClient) SetDelivery(d RemoteDelivery) { c.mu.Lock(); c.delivery = d; c.mu.Unlock() }
func (c *RemoteCoreClient) CoreEpoch() cluster.CoreEpoch {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.epoch
}
func (c *RemoteCoreClient) EdgeInstance() cluster.EdgeInstanceRef {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.instance
}

func (c *RemoteCoreClient) Start(ctx context.Context) error {
	backoff := c.cfg.ReconnectMin
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.done:
			return nil
		default:
		}
		if err := c.connectAndServe(ctx); err == nil {
			backoff = c.cfg.ReconnectMin
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		case <-c.done:
			return nil
		}
		backoff *= 2
		if backoff > c.cfg.ReconnectMax {
			backoff = c.cfg.ReconnectMax
		}
	}
}

func (c *RemoteCoreClient) connectAndServe(ctx context.Context) error {
	if c.cfg.TLSConfig == nil {
		return errors.New("remote core requires mTLS configuration")
	}
	d := tls.Dialer{Config: c.cfg.TLSConfig}
	raw, err := d.DialContext(ctx, "tcp", c.cfg.Address)
	if err != nil {
		return err
	}
	if err = cluster.WriteWire(raw, cluster.WireHello, 1, cluster.Hello{Version: cluster.ProtocolVersion{Major: cluster.WireMajor, Minor: cluster.WireMinor}, EdgeID: c.cfg.EdgeID, Capabilities: []string{"control", "voice"}}); err != nil {
		_ = raw.Close()
		return err
	}
	env, err := cluster.ReadWire(raw)
	if err != nil {
		_ = raw.Close()
		return err
	}
	if env.Type != cluster.WireHelloAck {
		_ = raw.Close()
		return cluster.ErrWireMessageType
	}
	var ack cluster.HelloAck
	if err = cluster.DecodeWire(env, &ack); err != nil || ack.Error != "" || ack.Version.Major != cluster.WireMajor {
		_ = raw.Close()
		if err != nil {
			return err
		}
		return fmt.Errorf("core rejected edge: %s", ack.Error)
	}
	c.mu.Lock()
	oldEpoch := c.epoch
	c.conn = raw
	c.instance = ack.EdgeInstance
	c.epoch = ack.CoreEpoch
	delivery := c.delivery
	c.mu.Unlock()
	if oldEpoch != "" && oldEpoch != ack.CoreEpoch && delivery != nil {
		delivery.CoreDisconnected(oldEpoch)
	}
	errCh := make(chan error, 2)
	connDone := make(chan struct{})
	var connDoneOnce sync.Once
	go func() { errCh <- c.writeLoop(raw, connDone) }()
	go func() { errCh <- c.readLoop(raw) }()
	ticker := time.NewTicker(c.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case err = <-errCh:
			connDoneOnce.Do(func() { close(connDone) })
			c.drop(raw, err)
			return err
		case <-ticker.C:
			if err = c.send(cluster.WireHeartbeat, 0, nil, false); err != nil {
				c.drop(raw, err)
				return err
			}
		case <-ctx.Done():
			connDoneOnce.Do(func() { close(connDone) })
			c.drop(raw, ctx.Err())
			return nil
		case <-c.done:
			connDoneOnce.Do(func() { close(connDone) })
			c.drop(raw, net.ErrClosed)
			return nil
		}
	}
}

func (c *RemoteCoreClient) drop(raw net.Conn, cause error) {
	c.mu.Lock()
	if c.conn == raw {
		c.conn = nil
		c.instance = cluster.EdgeInstanceRef{}
		d := c.delivery
		c.mu.Unlock()
		if d != nil {
			d.CoreDisconnected(c.epoch)
		}
	} else {
		c.mu.Unlock()
	}
	_ = raw.Close()
	for {
		select {
		case o := <-c.out:
			if o.done != nil {
				o.done <- cause
			}
		default:
			goto drained
		}
	}
drained:
	c.pendingMu.Lock()
	for id, ch := range c.pending {
		ch <- pendingResult{err: cause}
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
}

func (c *RemoteCoreClient) writeLoop(raw net.Conn, connDone <-chan struct{}) error {
	for {
		select {
		case o := <-c.out:
			_ = raw.SetWriteDeadline(time.Now().Add(c.cfg.HeartbeatTimeout))
			err := cluster.WriteWire(raw, o.typ, o.id, o.value)
			if o.done != nil {
				o.done <- err
			}
			if err != nil {
				return err
			}
		case <-c.done:
			return net.ErrClosed
		case <-connDone:
			return net.ErrClosed
		}
	}
}
func (c *RemoteCoreClient) readLoop(raw net.Conn) error {
	for {
		_ = raw.SetReadDeadline(time.Now().Add(c.cfg.HeartbeatTimeout))
		env, err := cluster.ReadWire(raw)
		if err != nil {
			return err
		}
		switch env.Type {
		case cluster.WireHeartbeat:
		case cluster.WireAuthResult:
			var resp cluster.AuthResponse
			if err = cluster.DecodeWire(env, &resp); err != nil {
				return err
			}
			c.pendingMu.Lock()
			ch := c.pending[env.ID]
			delete(c.pending, env.ID)
			c.pendingMu.Unlock()
			if ch != nil {
				ch <- pendingResult{response: resp}
			}
		case cluster.WireControlDown:
			var msg cluster.ControlWire
			if err = cluster.DecodeWire(env, &msg); err != nil {
				return err
			}
			c.mu.RLock()
			d := c.delivery
			c.mu.RUnlock()
			if d != nil {
				d.DeliverControl(msg)
			}
		case cluster.WireControlFlush:
			var ev cluster.SessionEvent
			if err = cluster.DecodeWire(env, &ev); err != nil {
				return err
			}
			c.mu.RLock()
			d := c.delivery
			c.mu.RUnlock()
			if d != nil {
				if err = d.FlushControl(ev.Session); err != nil {
					return err
				}
			}
			if err = c.send(cluster.WireControlFlushed, env.ID, ev, true); err != nil {
				return err
			}
		case cluster.WireVoiceDown:
			var v cluster.VoiceDown
			if err = cluster.DecodeWire(env, &v); err != nil {
				return err
			}
			c.mu.RLock()
			d := c.delivery
			c.mu.RUnlock()
			if d != nil {
				d.DeliverVoice(v.Batch)
			}
		case cluster.WireCloseSession:
			var ev cluster.SessionEvent
			if err = cluster.DecodeWire(env, &ev); err != nil {
				return err
			}
			c.mu.RLock()
			d := c.delivery
			c.mu.RUnlock()
			if d != nil {
				d.CloseSession(ev.Session)
			}
		default:
			return cluster.ErrWireMessageType
		}
	}
}

func (c *RemoteCoreClient) send(t cluster.WireType, id uint64, value any, reliable bool) error {
	c.mu.RLock()
	connected := c.conn != nil
	c.mu.RUnlock()
	if !connected {
		return ErrCoreProtocolNotImplemented
	}
	o := clientOut{typ: t, id: id, value: value}
	if reliable {
		o.done = make(chan error, 1)
	}
	select {
	case c.out <- o:
	case <-c.done:
		return net.ErrClosed
	default:
		return errors.New("remote core outbound queue full")
	}
	if o.done != nil {
		return <-o.done
	}
	return nil
}

func (c *RemoteCoreClient) Authenticate(ctx context.Context, f AuthForward) (AuthResult, error) {
	id := c.nextID.Add(1)
	ch := make(chan pendingResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	req := cluster.AuthRequest{ConnectionID: uint64(f.ConnID), Username: f.Username, Password: f.Password, Tokens: append([]string(nil), f.Tokens...), CertHash: f.CertHash, CertificateVerified: f.CertificateVerified, RemoteIP: f.RemoteIP, ClientVersion: f.ClientVersion, ClientCryptoModes: f.ClientCryptoModes}
	if err := c.send(cluster.WireAuthRequest, id, req, false); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return AuthResult{}, err
	}
	select {
	case got := <-ch:
		if got.err != nil {
			return AuthResult{}, got.err
		}
		r := got.response
		if !r.Rejected {
			c.mu.Lock()
			c.sessions[f.ConnID] = r.Session
			c.mu.Unlock()
		}
		return AuthResult{SessionID: r.Session.SessionID, Generation: r.Session.Generation, Username: r.Username, Rejected: r.Rejected, RejectType: messages.RejectType(r.RejectType), RejectReason: r.Reason}, nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return AuthResult{}, ctx.Err()
	case <-c.done:
		return AuthResult{}, net.ErrClosed
	}
}

func (c *RemoteCoreClient) ForwardControl(ctx context.Context, connID uint32, t protocol.MessageType, payload []byte) error {
	c.mu.RLock()
	connected := c.conn != nil
	c.mu.RUnlock()
	if !connected {
		return ErrCoreProtocolNotImplemented
	}
	c.mu.RLock()
	ref := c.sessions[connID]
	c.mu.RUnlock()
	if !ref.Valid() {
		return cluster.ErrStaleSession
	}
	return c.send(cluster.WireControlUp, 0, cluster.ControlWire{ConnectionID: uint64(connID), Session: ref, MessageType: uint16(t), Payload: append([]byte(nil), payload...)}, false)
}
func (c *RemoteCoreClient) TransportReady(ctx context.Context, connID uint32, ref cluster.SessionRef) error {
	return c.transportReadyWithMode(ctx, connID, ref, "")
}
func (c *RemoteCoreClient) transportReadyWithMode(ctx context.Context, connID uint32, ref cluster.SessionRef, mode string) error {
	return c.send(cluster.WireTransportReady, 0, cluster.SessionEvent{ConnectionID: uint64(connID), Session: ref, CryptoMode: mode}, true)
}
func (c *RemoteCoreClient) VoiceUp(ctx context.Context, ref cluster.SessionRef, f audio.Frame) error {
	f.OpusData = append([]byte(nil), f.OpusData...)
	return c.send(cluster.WireVoiceUp, 0, cluster.VoiceUp{Session: ref, Frame: f}, false)
}
func (c *RemoteCoreClient) Disconnect(ref cluster.SessionRef) {
	_ = c.send(cluster.WireDisconnect, 0, cluster.SessionEvent{Session: ref}, false)
}
func (c *RemoteCoreClient) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.mu.Lock()
		if c.conn != nil {
			_ = c.conn.Close()
		}
		c.mu.Unlock()
	})
	return nil
}

var _ RemoteCoreTransport = (*RemoteCoreClient)(nil)
var _ = json.RawMessage{}
