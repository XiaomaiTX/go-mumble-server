package app

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/edge"
	"github.com/dchote/go-mumble-server/internal/transport"
	"golang.org/x/sync/errgroup"
)

// buildEdgeRuntime composes edge mode: the shared client transport plus the
// edge-local runtime, with every Core-owned decision delegated to the
// RemoteCore client. No database is opened, no user/channel/ACL/ban/identity
// state, no Core voice router, no REST plane — a missing or unreachable Core
// fails closed instead of being substituted by a local one.
func buildEdgeRuntime(ctx context.Context, cfg *config.Config) (*App, error) {
	var tlsConfig *tls.Config
	var err error
	if cfg.CoreCACertPath != "" && cfg.EdgeClientCertPath != "" {
		tlsConfig, err = edgeCoreTLS(cfg)
		if err != nil {
			return nil, err
		}
	}
	remoteCore, err := edge.NewRemoteCoreClientWithConfig(edge.RemoteCoreClientConfig{Address: cfg.CoreAddress, EdgeID: cluster.EdgeID(cfg.EdgeID), TLSConfig: tlsConfig, ReconnectMin: cfg.EdgeReconnectMin, ReconnectMax: cfg.EdgeReconnectMax, HeartbeatInterval: cfg.EdgeHeartbeatInterval, HeartbeatTimeout: cfg.EdgeHeartbeatTimeout, QueueSize: cfg.EdgeQueueSize})
	if err != nil {
		return nil, fmt.Errorf("remote core client: %w", err)
	}
	certPEM, keyPEM, err := transport.LoadOrGenerateCert(cfg.SSLCertPath, cfg.SSLKeyPath)
	if err != nil {
		return nil, err
	}
	ert := edge.NewEdgeRuntime(cfg, remoteCore)
	cr := edge.NewClientRuntime(edge.ClientRuntimeConfig{
		Addr:      formatAddr(cfg.Host, cfg.MumblePort),
		CertPEM:   certPEM,
		KeyPEM:    keyPEM,
		Ingress:   edge.NewClientIngressRouter(ert.HandleEdgeMessage, ert.HandleCoreMessage),
		HandleUDP: ert.HandleUDP,
		SetupConn: ert.SetupConn,
		OnClose:   ert.OnClose,
	})
	if err := cr.Listen(ctx); err != nil {
		return nil, err
	}
	ert.BindUDP(cr.UDPConn())
	return &App{
		Mode:          config.ModeEdge,
		EdgeRuntime:   ert,
		RemoteCore:    remoteCore,
		ClientRuntime: cr,
		runtime:       &edgeModeRuntime{client: cr, remoteCore: remoteCore},
	}, nil
}

// edgeModeRuntime runs the edge process lifecycle.
type edgeModeRuntime struct {
	client     *edge.ClientRuntime
	remoteCore edge.RemoteCore
}

func (e *edgeModeRuntime) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return e.client.Run(gctx) })
	if transport, ok := e.remoteCore.(edge.RemoteCoreTransport); ok {
		g.Go(func() error { return transport.Start(gctx) })
	}
	return g.Wait()
}

func (e *edgeModeRuntime) Shutdown(_ context.Context) error {
	_ = e.remoteCore.Close()
	return e.client.Close()
}

func formatAddr(host string, port int) string {
	if host == "" {
		host = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d", host, port)
}
