package edge

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// ErrCoreProtocolNotImplemented marks every edge-core network operation in
// this phase. The edge fails closed on it instead of silently substituting a
// local Core; the real protocol (wire format, mTLS, heartbeats) lands in the
// next phase.
var ErrCoreProtocolNotImplemented = errors.New("edge-core network protocol is not implemented in this phase")

// RemoteCore is the typed Core/Edge boundary the EdgeRuntime depends on:
// authoritative authentication decisions and relayed core-owned control
// messages. Tests may substitute fakes; production uses RemoteCoreClient.
type RemoteCore interface {
	// Authenticate forwards the edge-collected halves of the split-owned
	// Authenticate message; the Core returns the authoritative session
	// decision.
	Authenticate(ctx context.Context, forward AuthForward) (AuthResult, error)
	// ForwardControl relays a core-owned control message from one edge-local
	// client connection.
	ForwardControl(ctx context.Context, connID uint32, msgType protocol.MessageType, payload []byte) error
	// Close releases the connection to the Core.
	Close() error
}

// AuthForward carries the edge-collected transport metadata together with the
// client's credentials: everything the Core needs to decide authentication
// without ever touching the edge's socket, CryptState or TLS objects.
type AuthForward struct {
	// ConnID is the edge-local ClientConnRef that owns the physical
	// connection; replies and closes address it. The cross-process wire
	// representation (plus CoreEpoch) lands with the network phase.
	ConnID uint32
	// Credentials from the Authenticate message.
	Username string
	Password string
	Tokens   []string
	// Edge-collected TLS/socket metadata.
	CertHash            string
	CertificateVerified bool
	RemoteIP            string
	// Core-owned copy of the split-owned client capabilities.
	ClientVersion     uint64
	ClientCryptoModes uint32
}

// AuthResult is the authoritative decision the Core returns for an
// AuthForward.
type AuthResult struct {
	// SessionID/Generation identify the logical session the Core created;
	// zero on rejection.
	SessionID  uint32
	Generation uint64
	// Rejected carries the Reject the edge must deliver as the connection's
	// final message.
	Rejected     bool
	RejectType   messages.RejectType
	RejectReason string
}

// RemoteCoreClient is the production RemoteCore: a network client for the
// authoritative Core. Phase 2A ships the skeleton — construction validates
// the configured Core address and trust anchor entry point, and every
// operation fails closed with ErrCoreProtocolNotImplemented so the edge never
// silently substitutes a local Core.
type RemoteCoreClient struct {
	address    string
	caCertPath string
}

// NewRemoteCoreClient validates the deployment's Core addressing.
func NewRemoteCoreClient(coreAddress, caCertPath string) (*RemoteCoreClient, error) {
	if coreAddress == "" {
		return nil, errors.New("remote core address is required")
	}
	if _, _, err := net.SplitHostPort(coreAddress); err != nil {
		return nil, fmt.Errorf("remote core address %q is not host:port: %w", coreAddress, err)
	}
	return &RemoteCoreClient{address: coreAddress, caCertPath: caCertPath}, nil
}

// Address reports the configured Core address.
func (c *RemoteCoreClient) Address() string { return c.address }

// Authenticate is not wired to the network yet (next phase).
func (c *RemoteCoreClient) Authenticate(context.Context, AuthForward) (AuthResult, error) {
	return AuthResult{}, ErrCoreProtocolNotImplemented
}

// ForwardControl is not wired to the network yet (next phase).
func (c *RemoteCoreClient) ForwardControl(_ context.Context, _ uint32, _ protocol.MessageType, _ []byte) error {
	return ErrCoreProtocolNotImplemented
}

// Close is a no-op while the transport is a skeleton.
func (c *RemoteCoreClient) Close() error { return nil }
