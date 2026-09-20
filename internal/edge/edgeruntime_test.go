package edge

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// fakeCore is a test RemoteCore: it records every forwarded operation and
// replays scripted results.
type fakeCore struct {
	mu         sync.Mutex
	auths      []AuthForward
	authResult AuthResult
	authErr    error
	forwards   []forwardedControl
	forwardErr error
}

type forwardedControl struct {
	connID  uint32
	msgType protocol.MessageType
	payload []byte
}

func (f *fakeCore) Authenticate(_ context.Context, fw AuthForward) (AuthResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auths = append(f.auths, fw)
	return f.authResult, f.authErr
}

func (f *fakeCore) ForwardControl(_ context.Context, connID uint32, msgType protocol.MessageType, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forwards = append(f.forwards, forwardedControl{connID: connID, msgType: msgType, payload: payload})
	return f.forwardErr
}

func (f *fakeCore) Close() error { return nil }

func (f *fakeCore) recordedAuths() []AuthForward {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AuthForward(nil), f.auths...)
}

func (f *fakeCore) recordedForwards() []forwardedControl {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forwardedControl(nil), f.forwards...)
}

func newEdgeRuntime(t *testing.T, core RemoteCore) (*EdgeRuntime, *config.Config) {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return NewEdgeRuntime(cfg, core), cfg
}

// newEdgeConn builds a running connection.Conn over a net.Pipe socket.
func newEdgeConn(t *testing.T) (*connection.Conn, net.Conn) {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	c := connection.New(serverSide, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.Run(ctx, protocol.NewHandlerTable()) }()
	t.Cleanup(func() {
		cancel()
		_ = clientSide.Close()
	})
	return c, clientSide
}

func readReject(t *testing.T, conn net.Conn) *messages.Reject {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	msgType, payload, err := protocol.ReadPacket(conn)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if msgType != protocol.MessageReject {
		t.Fatalf("message type = %d, want Reject (%d)", msgType, protocol.MessageReject)
	}
	var reject messages.Reject
	if err := reject.Unmarshal(payload); err != nil {
		t.Fatalf("Unmarshal Reject: %v", err)
	}
	return &reject
}

func authenticatePayload(t *testing.T, username string) []byte {
	t.Helper()
	payload, err := (&messages.Authenticate{Username: username, Password: "pw", Tokens: []string{"t1"}}).Marshal()
	if err != nil {
		t.Fatalf("Marshal Authenticate: %v", err)
	}
	return payload
}

// An empty username is rejected by the edge itself; the Core is never asked.
func TestEdgeAuthenticateRejectsEmptyUsernameLocally(t *testing.T) {
	er, _ := newEdgeRuntime(t, &fakeCore{})
	c, clientSide := newEdgeConn(t)

	if err := er.HandleEdgeMessage(protocol.MessageAuthenticate, authenticatePayload(t, ""), c); err != nil {
		t.Fatalf("HandleEdgeMessage: %v", err)
	}
	reject := readReject(t, clientSide)
	if reject.Type != messages.RejectInvalidUsername {
		t.Fatalf("reject type = %d, want RejectInvalidUsername", reject.Type)
	}
	if c.SessionID() != 0 {
		t.Fatalf("rejected conn got session id %d, want 0", c.SessionID())
	}
}

// The skeleton remote core fails closed: the client gets the standard
// authenticator rejection, never a locally-substituted Core.
func TestEdgeAuthenticateFailsClosedWithoutCoreProtocol(t *testing.T) {
	fc := &fakeCore{authErr: ErrCoreProtocolNotImplemented}
	er, _ := newEdgeRuntime(t, fc)
	c, clientSide := newEdgeConn(t)

	if err := er.HandleEdgeMessage(protocol.MessageAuthenticate, authenticatePayload(t, "bob"), c); err != nil {
		t.Fatalf("HandleEdgeMessage: %v", err)
	}
	reject := readReject(t, clientSide)
	if reject.Type != messages.RejectAuthenticatorFail {
		t.Fatalf("reject type = %d, want RejectAuthenticatorFail", reject.Type)
	}
}

// A Core-side rejection is relayed verbatim as the final message.
func TestEdgeAuthenticateRelaysCoreRejection(t *testing.T) {
	fc := &fakeCore{authResult: AuthResult{
		Rejected:     true,
		RejectType:   messages.RejectWrongServerPW,
		RejectReason: "bad password",
	}}
	er, _ := newEdgeRuntime(t, fc)
	c, clientSide := newEdgeConn(t)

	if err := er.HandleEdgeMessage(protocol.MessageAuthenticate, authenticatePayload(t, "bob"), c); err != nil {
		t.Fatalf("HandleEdgeMessage: %v", err)
	}
	reject := readReject(t, clientSide)
	if reject.Type != messages.RejectWrongServerPW || reject.Reason != "bad password" {
		t.Fatalf("relayed reject = %+v, want WrongServerPW/bad password", reject)
	}
}

// On acceptance the edge adopts the Core's session identity and forwards the
// edge-collected halves (credentials + transport metadata, never socket
// objects) with its ClientConnRef.
func TestEdgeAuthenticateForwardsMetadataAndAdoptsSession(t *testing.T) {
	fc := &fakeCore{authResult: AuthResult{SessionID: 42, Generation: 7}}
	er, _ := newEdgeRuntime(t, fc)
	c, clientSide := newEdgeConn(t)

	versionPayload, err := (&messages.Version{VersionV2: 1 << 48, CryptoModes: 0x05}).Marshal()
	if err != nil {
		t.Fatalf("Marshal Version: %v", err)
	}
	if err := er.HandleEdgeMessage(protocol.MessageVersion, versionPayload, c); err != nil {
		t.Fatalf("HandleEdgeMessage(Version): %v", err)
	}
	if err := er.HandleEdgeMessage(protocol.MessageAuthenticate, authenticatePayload(t, "bob"), c); err != nil {
		t.Fatalf("HandleEdgeMessage(Authenticate): %v", err)
	}
	_ = clientSide

	auths := fc.recordedAuths()
	if len(auths) != 1 {
		t.Fatalf("core saw %d auth forwards, want 1", len(auths))
	}
	fw := auths[0]
	if fw.Username != "bob" || fw.Password != "pw" || len(fw.Tokens) != 1 || fw.Tokens[0] != "t1" {
		t.Fatalf("forwarded credentials = %+v", fw)
	}
	if fw.ConnID == 0 {
		t.Fatal("forward lacked the edge-local ClientConnRef")
	}
	if fw.ClientVersion != 1<<48 || fw.ClientCryptoModes != 0x05 {
		t.Fatalf("forwarded client capabilities = version %#x modes %#x", fw.ClientVersion, fw.ClientCryptoModes)
	}
	if c.SessionID() != fw.ConnID {
		t.Fatalf("conn SessionID = %d, want the edge-local ref %d", c.SessionID(), fw.ConnID)
	}
	cs, ok := er.coreSessionOf(fw.ConnID)
	if !ok || cs.sessionID != 42 || cs.generation != 7 {
		t.Fatalf("core identity for ref %d = %+v ok=%v, want (42, 7)", fw.ConnID, cs, ok)
	}
	if c.UserName() != "bob" {
		t.Fatalf("conn UserName = %q, want bob", c.UserName())
	}

	// A retried Authenticate on the same connection must not leak a second
	// ClientConnRef: attach is idempotent.
	if err := er.HandleEdgeMessage(protocol.MessageAuthenticate, authenticatePayload(t, "bob"), c); err != nil {
		t.Fatalf("retried HandleEdgeMessage: %v", err)
	}
	if got := len(fc.recordedAuths()); got != 2 {
		t.Fatalf("retries forwarded = %d, want 2", got)
	}
	if fc.recordedAuths()[1].ConnID != fw.ConnID {
		t.Fatal("retry was assigned a new ClientConnRef")
	}
}

// Core-owned messages from a connection that never authenticated are dropped
// at the edge instead of being forwarded.
func TestEdgeCoreMessagesNeedAuthenticatedConn(t *testing.T) {
	fc := &fakeCore{}
	er, _ := newEdgeRuntime(t, fc)
	c, _ := newEdgeConn(t)

	payload, err := (&messages.TextMessage{Message: "hi"}).Marshal()
	if err != nil {
		t.Fatalf("Marshal TextMessage: %v", err)
	}
	if err := er.HandleCoreMessage(protocol.MessageTextMessage, payload, c); err != nil {
		t.Fatalf("HandleCoreMessage: %v", err)
	}
	if got := len(fc.recordedForwards()); got != 0 {
		t.Fatalf("unauthenticated forward reached the core %d times", got)
	}
}

// Core-owned messages from an authenticated connection are relayed tagged
// with the edge-local ClientConnRef.
func TestEdgeCoreMessagesForwardWithTaggedRef(t *testing.T) {
	fc := &fakeCore{authResult: AuthResult{SessionID: 42, Generation: 7}}
	er, _ := newEdgeRuntime(t, fc)
	c, _ := newEdgeConn(t)

	if err := er.HandleEdgeMessage(protocol.MessageAuthenticate, authenticatePayload(t, "bob"), c); err != nil {
		t.Fatalf("HandleEdgeMessage: %v", err)
	}
	payload, err := (&messages.TextMessage{Message: "hi"}).Marshal()
	if err != nil {
		t.Fatalf("Marshal TextMessage: %v", err)
	}
	if err := er.HandleCoreMessage(protocol.MessageTextMessage, payload, c); err != nil {
		t.Fatalf("HandleCoreMessage: %v", err)
	}
	forwards := fc.recordedForwards()
	if len(forwards) != 1 {
		t.Fatalf("core saw %d forwards, want 1", len(forwards))
	}
	if forwards[0].msgType != protocol.MessageTextMessage || string(forwards[0].payload) != string(payload) {
		t.Fatalf("forward = %+v, want verbatim TextMessage", forwards[0])
	}
	if forwards[0].connID != c.SessionID() {
		t.Fatalf("forward tagged connID %d, want %d", forwards[0].connID, c.SessionID())
	}
}

// The edge half of the split-owned Version exchange records the client's
// capabilities on the connection.
func TestEdgeVersionRecordsClientCapabilities(t *testing.T) {
	er, _ := newEdgeRuntime(t, &fakeCore{})
	c, _ := newEdgeConn(t)

	versionPayload, err := (&messages.Version{VersionV1: 1<<16 | 5<<8, Release: "test"}).Marshal()
	if err != nil {
		t.Fatalf("Marshal Version: %v", err)
	}
	if err := er.HandleEdgeMessage(protocol.MessageVersion, versionPayload, c); err != nil {
		t.Fatalf("HandleEdgeMessage(Version): %v", err)
	}
	if got := c.ClientVersion(); got != ClientVersionFull(messages.Version{VersionV1: 1<<16 | 5<<8}) {
		t.Fatalf("ClientVersion = %#x", got)
	}
}

// UDPTunnel frames are only decoded for active connections; malformed frames
// fail visibly instead of being silently dropped.
func TestEdgeUDPTunnelValidatesActiveFrames(t *testing.T) {
	er, _ := newEdgeRuntime(t, &fakeCore{})
	c, _ := newEdgeConn(t)

	// Not active yet: the frame is ignored.
	if err := er.HandleEdgeMessage(protocol.MessageUDPTunnel, []byte{0xFF}, c); err != nil {
		t.Fatalf("HandleEdgeMessage before active: %v", err)
	}
	c.SetActive()
	if err := er.HandleEdgeMessage(protocol.MessageUDPTunnel, []byte{0xFF}, c); err == nil {
		t.Fatal("malformed active UDPTunnel frame was accepted")
	}
}

// The edge answers unencrypted server-list probes locally, exactly like the
// Local Edge does.
func TestEdgeHandleUDPAnswersServerListProbe(t *testing.T) {
	er, cfg := newEdgeRuntime(t, &fakeCore{})
	cfg.MaxUsers = 100
	cfg.MaxBandwidth = 128000

	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen server udp: %v", err)
	}
	t.Cleanup(func() { _ = serverConn.Close() })
	probeConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen probe udp: %v", err)
	}
	t.Cleanup(func() { _ = probeConn.Close() })
	er.BindUDP(serverConn)

	req := make([]byte, 12)
	binary.BigEndian.PutUint64(req[4:12], 0x0123456789ABCDEF)
	er.HandleUDP(probeConn.LocalAddr(), req)

	buf := make([]byte, 64)
	if err := probeConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	n, _, err := probeConn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if n != 24 || string(buf[4:12]) != string(req[4:12]) {
		t.Fatalf("legacy reply = % x", buf[:n])
	}
	// No session state exists, so the reported user count is zero.
	if v := binary.BigEndian.Uint32(buf[12:16]); v != 0 {
		t.Fatalf("user count = %d, want 0", v)
	}
}

// OnClose releases the edge-local ClientConnRef and nothing else — Core-side
// teardown is not the edge's business.
func TestEdgeOnCloseReleasesConnRef(t *testing.T) {
	fc := &fakeCore{authResult: AuthResult{SessionID: 42, Generation: 7}}
	er, _ := newEdgeRuntime(t, fc)
	c, _ := newEdgeConn(t)
	if err := er.HandleEdgeMessage(protocol.MessageAuthenticate, authenticatePayload(t, "bob"), c); err != nil {
		t.Fatalf("HandleEdgeMessage: %v", err)
	}

	er.OnClose(c)
	if err := er.HandleCoreMessage(protocol.MessageTextMessage, []byte{}, c); err != nil {
		t.Fatalf("HandleCoreMessage after close: %v", err)
	}
	if got := len(fc.recordedForwards()); got != 0 {
		t.Fatalf("closed conn still forwarded %d messages", got)
	}
	if _, ok := er.coreSessionOf(c.SessionID()); ok {
		t.Fatal("core identity outlived the connection")
	}
}

// The production RemoteCoreClient fails closed on every operation.
func TestRemoteCoreClientFailsClosed(t *testing.T) {
	client, err := NewRemoteCoreClient("127.0.0.1:64740", "")
	if err != nil {
		t.Fatalf("NewRemoteCoreClient: %v", err)
	}
	if _, err := client.Authenticate(context.Background(), AuthForward{}); !errors.Is(err, ErrCoreProtocolNotImplemented) {
		t.Fatalf("Authenticate err = %v, want ErrCoreProtocolNotImplemented", err)
	}
	if err := client.ForwardControl(context.Background(), 1, protocol.MessageTextMessage, nil); !errors.Is(err, ErrCoreProtocolNotImplemented) {
		t.Fatalf("ForwardControl err = %v, want ErrCoreProtocolNotImplemented", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNewRemoteCoreClientValidatesAddress(t *testing.T) {
	if _, err := NewRemoteCoreClient("", ""); err == nil {
		t.Fatal("empty address accepted")
	}
	if _, err := NewRemoteCoreClient("no-port", ""); err == nil {
		t.Fatal("address without port accepted")
	}
}
