package cluster

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type RemoteEdgeBackend interface {
	EdgeConnected(EdgeInstanceRef, VoiceTransport)
	AuthenticateRemote(context.Context, EdgeInstanceRef, AuthRequest) AuthResponse
	TransportReadyRemote(context.Context, EdgeInstanceRef, SessionEvent, ControlSink) error
	ControlRemote(context.Context, EdgeInstanceRef, ControlWire) error
	VoiceRemote(context.Context, EdgeInstanceRef, VoiceUp) error
	DisconnectRemote(EdgeInstanceRef, SessionEvent)
	EdgeDisconnected(EdgeInstanceRef)
}

type RemoteEdgeServerConfig struct {
	ListenAddr        string
	TLSConfig         *tls.Config
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	QueueSize         int
}

type RemoteEdgeServer struct {
	registry  *Registry
	backend   RemoteEdgeBackend
	cfg       RemoteEdgeServerConfig
	epoch     CoreEpoch
	mu        sync.Mutex
	registerMu sync.Mutex
	listener  net.Listener
	instances map[EdgeID]*edgeWireConn
	nextGen   map[EdgeID]uint64
	closeOnce sync.Once
}

func NewRemoteEdgeServer(registry *Registry, listenAddr string) *RemoteEdgeServer {
	return NewRemoteEdgeServerWithConfig(registry, nil, RemoteEdgeServerConfig{ListenAddr: listenAddr})
}

func NewRemoteEdgeServerWithConfig(registry *Registry, backend RemoteEdgeBackend, cfg RemoteEdgeServerConfig) *RemoteEdgeServer {
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 5 * time.Second
	}
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = 15 * time.Second
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	var token [16]byte
	_, _ = rand.Read(token[:])
	return &RemoteEdgeServer{registry: registry, backend: backend, cfg: cfg,
		epoch: CoreEpoch(hex.EncodeToString(token[:])), instances: make(map[EdgeID]*edgeWireConn), nextGen: make(map[EdgeID]uint64)}
}

func (s *RemoteEdgeServer) Registry() *Registry  { return s.registry }
func (s *RemoteEdgeServer) ListenAddr() string   { return s.cfg.ListenAddr }
func (s *RemoteEdgeServer) CoreEpoch() CoreEpoch { return s.epoch }
func (s *RemoteEdgeServer) Addr() net.Addr {
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *RemoteEdgeServer) Start(ctx context.Context) error {
	if s.cfg.ListenAddr == "" {
		return nil
	}
	if s.cfg.TLSConfig == nil {
		if s.backend == nil {
			return nil
		}
		return errors.New("remote edge server requires mTLS configuration")
	}
	ln, err := tls.Listen("tcp", s.cfg.ListenAddr, s.cfg.TLSConfig)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	slog.Info("Edge-Core mTLS listening", "addr", ln.Addr(), "core_epoch", s.epoch)
	go func() { <-ctx.Done(); _ = s.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.serve(ctx, conn.(*tls.Conn))
	}
}

func (s *RemoteEdgeServer) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.listener != nil {
			_ = s.listener.Close()
			s.listener = nil
		}
		conns := make([]*edgeWireConn, 0, len(s.instances))
		for _, c := range s.instances {
			conns = append(conns, c)
		}
		s.mu.Unlock()
		for _, c := range conns {
			c.close()
		}
	})
	return nil
}

func (s *RemoteEdgeServer) serve(ctx context.Context, tc *tls.Conn) {
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = tc.Close()
		return
	}
	env, err := ReadWire(tc)
	if err != nil || env.Type != WireHello {
		_ = tc.Close()
		return
	}
	var hello Hello
	if DecodeWire(env, &hello) != nil || hello.Version.Major != WireMajor || hello.EdgeID == "" {
		_ = WriteWire(tc, WireHelloAck, env.ID, HelloAck{Version: ProtocolVersion{Major: WireMajor, Minor: WireMinor}, Error: "incompatible protocol"})
		_ = tc.Close()
		return
	}
	if !peerMatchesEdge(tc.ConnectionState(), hello.EdgeID) {
		_ = tc.Close()
		return
	}

	s.mu.Lock()
	s.nextGen[hello.EdgeID]++
	instance := EdgeInstanceRef{EdgeID: hello.EdgeID, Generation: s.nextGen[hello.EdgeID]}
	old := s.instances[hello.EdgeID]
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	if err = s.registry.RegisterEdge(hello.EdgeID, instance.Generation); err != nil {
		_ = tc.Close()
		return
	}
	ec := newEdgeWireConn(s, tc, instance)
	s.mu.Lock()
	s.instances[hello.EdgeID] = ec
	s.mu.Unlock()
	if s.backend != nil {
		s.backend.EdgeConnected(instance, ec)
	}
	if err = ec.send(WireHelloAck, env.ID, HelloAck{Version: ProtocolVersion{Major: WireMajor, Minor: WireMinor}, CoreEpoch: s.epoch, EdgeInstance: instance, Capabilities: []string{"control", "voice"}}, true); err != nil {
		ec.close()
		return
	}
	s.mu.Lock()
	current := s.instances[instance.EdgeID] == ec
	s.mu.Unlock()
	if current {
		ec.run(ctx)
	} else {
		ec.close()
	}
}

func peerMatchesEdge(st tls.ConnectionState, edgeID EdgeID) bool {
	if len(st.VerifiedChains) == 0 || len(st.PeerCertificates) == 0 {
		return false
	}
	cert := st.PeerCertificates[0]
	want := string(edgeID)
	if strings.EqualFold(cert.Subject.CommonName, want) {
		return true
	}
	for _, name := range cert.DNSNames {
		if strings.EqualFold(name, want) {
			return true
		}
	}
	return false
}

type wireOut struct {
	typ   WireType
	id    uint64
	value any
	done  chan error
}
type edgeWireConn struct {
	server    *RemoteEdgeServer
	conn      net.Conn
	instance  EdgeInstanceRef
	out       chan wireOut
	done      chan struct{}
	once      sync.Once
	lastSeen  atomic.Int64
	nextID    atomic.Uint64
	flushMu   sync.Mutex
	flushWait map[uint64]chan error
	sessions  map[uint64]SessionRef
}

func newEdgeWireConn(s *RemoteEdgeServer, c net.Conn, instance EdgeInstanceRef) *edgeWireConn {
	e := &edgeWireConn{server: s, conn: c, instance: instance, out: make(chan wireOut, s.cfg.QueueSize), done: make(chan struct{}), flushWait: make(map[uint64]chan error), sessions: make(map[uint64]SessionRef)}
	e.lastSeen.Store(time.Now().UnixNano())
	go e.writeLoop()
	return e
}

func (e *edgeWireConn) send(t WireType, id uint64, value any, reliable bool) error {
	o := wireOut{typ: t, id: id, value: value}
	if reliable {
		o.done = make(chan error, 1)
	}
	select {
	case <-e.done:
		return net.ErrClosed
	case e.out <- o:
	default:
		return fmt.Errorf("edge %s outbound queue full", e.instance.EdgeID)
	}
	if o.done != nil {
		select {
		case err := <-o.done:
			return err
		case <-e.done:
			return net.ErrClosed
		}
	}
	return nil
}

func (e *edgeWireConn) writeLoop() {
	for {
		select {
		case o := <-e.out:
			_ = e.conn.SetWriteDeadline(time.Now().Add(e.server.cfg.HeartbeatTimeout))
			err := WriteWire(e.conn, o.typ, o.id, o.value)
			if o.done != nil {
				o.done <- err
			}
			if err != nil {
				e.close()
				return
			}
		case <-e.done:
			return
		}
	}
}

func (e *edgeWireConn) run(ctx context.Context) {
	ticker := time.NewTicker(e.server.cfg.HeartbeatInterval)
	defer ticker.Stop()
	defer e.close()
	go func() {
		for {
			select {
			case <-ticker.C:
				if time.Since(time.Unix(0, e.lastSeen.Load())) > e.server.cfg.HeartbeatTimeout {
					e.close()
					return
				}
				_ = e.send(WireHeartbeat, 0, nil, false)
			case <-e.done:
				return
			case <-ctx.Done():
				e.close()
				return
			}
		}
	}()
	for {
		env, err := ReadWire(e.conn)
		if err != nil {
			return
		}
		e.lastSeen.Store(time.Now().UnixNano())
		if env.Type == WireHeartbeat {
			continue
		}
		if err = e.handle(ctx, env); err != nil {
			slog.Warn("invalid edge-core message", "edge", e.instance.EdgeID, "error", err)
			return
		}
	}
}

func (e *edgeWireConn) handle(ctx context.Context, env WireEnvelope) error {
	b := e.server.backend
	if b == nil {
		return errors.New("remote edge backend unavailable")
	}
	switch env.Type {
	case WireAuthRequest:
		var req AuthRequest
		if err := DecodeWire(env, &req); err != nil {
			return err
		}
		if req.ConnectionID == 0 {
			return ErrInvalidSession
		}
		resp := b.AuthenticateRemote(ctx, e.instance, req)
		if !resp.Rejected {
			if _, exists := e.sessions[req.ConnectionID]; exists {
				return ErrInvalidSession
			}
			e.sessions[req.ConnectionID] = resp.Session
		}
		return e.send(WireAuthResult, env.ID, resp, true)
	case WireTransportReady:
		var event SessionEvent
		if err := DecodeWire(env, &event); err != nil {
			return err
		}
		if e.sessions[event.ConnectionID] != event.Session {
			return ErrStaleSession
		}
		go func() {
			if err := b.TransportReadyRemote(ctx, e.instance, event, &remoteControlSink{edge: e, session: event.Session}); err != nil {
				slog.Warn("remote session sync failed", "edge", e.instance.EdgeID, "session", event.Session.SessionID, "error", err)
			}
		}()
		return nil
	case WireControlUp:
		var msg ControlWire
		if err := DecodeWire(env, &msg); err != nil {
			return err
		}
		if e.sessions[msg.ConnectionID] != msg.Session {
			return ErrStaleSession
		}
		return b.ControlRemote(ctx, e.instance, msg)
	case WireControlFlushed:
		e.flushMu.Lock()
		ch := e.flushWait[env.ID]
		delete(e.flushWait, env.ID)
		e.flushMu.Unlock()
		if ch == nil {
			return ErrInvalidSession
		}
		ch <- nil
		return nil
	case WireVoiceUp:
		var voice VoiceUp
		if err := DecodeWire(env, &voice); err != nil {
			return err
		}
		return b.VoiceRemote(ctx, e.instance, voice)
	case WireDisconnect:
		var event SessionEvent
		if err := DecodeWire(env, &event); err != nil {
			return err
		}
		for id, ref := range e.sessions {
			if ref == event.Session {
				delete(e.sessions, id)
			}
		}
		b.DisconnectRemote(e.instance, event)
		return nil
	default:
		return ErrWireMessageType
	}
}

func (e *edgeWireConn) close() {
	e.once.Do(func() {
		close(e.done)
		_ = e.conn.Close()
		e.flushMu.Lock()
		for id, ch := range e.flushWait {
			ch <- net.ErrClosed
			delete(e.flushWait, id)
		}
		e.flushMu.Unlock()
		e.server.mu.Lock()
		if e.server.instances[e.instance.EdgeID] == e {
			delete(e.server.instances, e.instance.EdgeID)
		}
		e.server.mu.Unlock()
		if e.server.backend != nil {
			e.server.backend.EdgeDisconnected(e.instance)
		}
		e.server.registry.RemoveEdge(e.instance.EdgeID, e.instance.Generation)
	})
}

type remoteControlSink struct {
	edge    *edgeWireConn
	session SessionRef
}

func (s *remoteControlSink) WriteControl(m ControlMessage) error {
	payload, err := m.Message.Marshal()
	if err != nil {
		return err
	}
	return s.edge.send(WireControlDown, 0, ControlWire{Session: s.session, MessageType: uint16(m.Type), Payload: append([]byte(nil), payload...)}, false)
}
func (s *remoteControlSink) Flush(ctx context.Context) error {
	id := s.edge.nextID.Add(1)
	ack := make(chan error, 1)
	s.edge.flushMu.Lock()
	s.edge.flushWait[id] = ack
	s.edge.flushMu.Unlock()
	if err := s.edge.send(WireControlFlush, id, SessionEvent{Session: s.session}, true); err != nil {
		s.edge.flushMu.Lock()
		delete(s.edge.flushWait, id)
		s.edge.flushMu.Unlock()
		return err
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		s.edge.flushMu.Lock()
		delete(s.edge.flushWait, id)
		s.edge.flushMu.Unlock()
		return ctx.Err()
	case <-s.edge.done:
		return net.ErrClosed
	}
}
func (s *remoteControlSink) Close() error {
	return s.edge.send(WireCloseSession, 0, SessionEvent{Session: s.session}, false)
}
func (e *edgeWireConn) DeliverVoice(_ context.Context, batch VoiceBatch) error {
	return e.send(WireVoiceDown, 0, VoiceDown{Batch: cloneVoiceBatch(batch)}, false)
}
func cloneVoiceBatch(b VoiceBatch) VoiceBatch {
	b.Frame.OpusData = append([]byte(nil), b.Frame.OpusData...)
	b.Recipients = append([]VoiceRecipient(nil), b.Recipients...)
	return b
}

var _ VoiceTransport = (*edgeWireConn)(nil)
var _ ControlSink = (*remoteControlSink)(nil)
