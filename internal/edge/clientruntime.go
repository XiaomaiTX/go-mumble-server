package edge

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/internal/transport"
	"github.com/dchote/go-mumble-server/pkg/mumble/crypto"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
	"golang.org/x/sync/errgroup"
)

// ClientRuntimeConfig parameterizes the shared client transport. Every
// protocol decision is delegated: Ingress routes by ownership, HandleUDP owns
// edge-local UDP semantics, SetupConn and OnClose carry the per-connection
// lifecycle glue of the composing edge.
type ClientRuntimeConfig struct {
	// Addr is the Mumble TCP+UDP bind address (host:port).
	Addr string
	// CertPEM/KeyPEM supply the TLS identity.
	CertPEM []byte
	KeyPEM  []byte
	// Ingress is the client message ownership router.
	Ingress *ClientIngressRouter
	// HandleUDP processes one inbound UDP datagram (edge-owned).
	HandleUDP func(net.Addr, []byte)
	// SetupConn runs once per accepted connection before the banner (e.g.
	// install the Core metric sink).
	SetupConn func(*connection.Conn)
	// OnClose is the physical disconnect cleanup.
	OnClose func(*connection.Conn)
	// VersionV1/V2 override the announced protocol capability; zero means
	// ProtocolVersionV1/V2.
	VersionV1 uint32
	VersionV2 uint64
}

// ClientRuntime owns the physical Mumble client transport: the TCP/TLS
// listener, the UDP socket, connection.Conn lifecycle and the server Version
// banner. It is shared verbatim by the Local Edge (standalone/core) and the
// edge-mode EdgeRuntime; it holds no protocol or business state of its own.
type ClientRuntime struct {
	cfg   ClientRuntimeConfig
	tcpLn net.Listener
	udp   *net.UDPConn
}

// NewClientRuntime builds the runtime; call Listen before Run.
func NewClientRuntime(cfg ClientRuntimeConfig) *ClientRuntime {
	return &ClientRuntime{cfg: cfg}
}

// Listen binds the TCP/TLS and UDP sockets.
func (r *ClientRuntime) Listen(ctx context.Context) error {
	tcpLn, err := transport.TCPListener(ctx, r.cfg.Addr, r.cfg.CertPEM, r.cfg.KeyPEM)
	if err != nil {
		return err
	}
	r.tcpLn = tcpLn
	udpConn, err := transport.UDPListener(ctx, r.cfg.Addr)
	if err != nil {
		_ = tcpLn.Close()
		return err
	}
	r.udp = udpConn
	return nil
}

// UDPConn exposes the UDP socket the edge-local handlers write replies
// through (ping echoes, voice). Nil before Listen.
func (r *ClientRuntime) UDPConn() net.PacketConn {
	if r.udp == nil {
		return nil
	}
	return r.udp
}

// Run serves the accept and UDP read loops until ctx is cancelled or a loop
// fails. TCP and UDP are bound to the same port, but the sockets are
// independent; Run returns nil on orderly shutdown.
func (r *ClientRuntime) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return r.acceptLoop(gctx) })
	g.Go(func() error { return r.udpReadLoop(gctx) })
	return g.Wait()
}

// Close tears down both sockets.
func (r *ClientRuntime) Close() error {
	var err error
	if r.tcpLn != nil {
		err = r.tcpLn.Close()
		r.tcpLn = nil
	}
	if r.udp != nil {
		if cerr := r.udp.Close(); err == nil {
			err = cerr
		}
		r.udp = nil
	}
	return err
}

// Addr reports the bound TCP address (useful when configured with port 0).
func (r *ClientRuntime) Addr() net.Addr {
	if r.tcpLn == nil {
		return nil
	}
	return r.tcpLn.Addr()
}

func (r *ClientRuntime) acceptLoop(ctx context.Context) error {
	slog.Info("Mumble TCP/TLS listening", "addr", r.cfg.Addr)
	for {
		raw, err := r.tcpLn.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		crypt := crypto.NewCryptState(crypto.ModeLegacy)
		remoteAddr := raw.RemoteAddr().String()
		slog.Info("Mumble client connected", "remote", remoteAddr)
		conn := connection.New(raw, crypt, r.cfg.OnClose)
		if r.cfg.SetupConn != nil {
			r.cfg.SetupConn(conn)
		}
		versionV1, versionV2 := r.cfg.VersionV1, r.cfg.VersionV2
		if versionV1 == 0 {
			versionV1 = ProtocolVersionV1
		}
		if versionV2 == 0 {
			versionV2 = ProtocolVersionV2
		}
		go func() {
			_ = conn.WriteMessage(protocol.MessageVersion, &messages.Version{
				VersionV1:   versionV1,
				VersionV2:   versionV2,
				Release:     BannerRelease,
				OS:          BannerOS,
				OSVersion:   BannerOSVersion,
				CryptoModes: BannerCryptoModes,
			})
			_ = conn.Run(ctx, r.ingressTable())
		}()
	}
}

func (r *ClientRuntime) ingressTable() protocol.HandlerTable {
	if r.cfg.Ingress == nil {
		return protocol.NewHandlerTable()
	}
	return r.cfg.Ingress.Table()
}

func (r *ClientRuntime) udpReadLoop(ctx context.Context) error {
	slog.Info("Mumble UDP listening", "addr", r.cfg.Addr)
	buf := make([]byte, 65535)
	for {
		r.udp.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, addr, err := r.udp.ReadFrom(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				select {
				case <-ctx.Done():
					return nil
				default:
					continue
				}
			}
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		if n > 0 && r.cfg.HandleUDP != nil {
			r.cfg.HandleUDP(addr, buf[:n])
		}
	}
}
