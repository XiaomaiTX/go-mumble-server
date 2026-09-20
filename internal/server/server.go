package server

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"

	"github.com/dchote/go-mumble-server/internal/cert"
	"github.com/dchote/go-mumble-server/internal/channel"
	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/database"
	"github.com/dchote/go-mumble-server/internal/discovery"
	"github.com/dchote/go-mumble-server/internal/edge"
	"github.com/dchote/go-mumble-server/internal/mumble"
	"github.com/dchote/go-mumble-server/internal/rest"
	"github.com/dchote/go-mumble-server/internal/transport"
	pkgmumble "github.com/dchote/go-mumble-server/pkg/mumble"
	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"
)

// Server is the mode-agnostic standalone/core process runtime: the in-process
// Core (mumble.Server), the Local Edge adapter, the shared client transport
// and the REST plane. Mode-specific composition — remote edge capability,
// remote core client — belongs to internal/app, never here.
type Server struct {
	cfg  *config.Config
	db   *gorm.DB
	feFS fs.FS

	mu      sync.Mutex
	http    *http.Server
	runtime *edge.ClientRuntime
	mdns    *discovery.Server

	// ms is the in-process Core; runCfg is the persisted-config-merged view
	// Run needs. Both are set by Prepare and read-only afterwards.
	ms     *mumble.Server
	runCfg *config.Config
}

// New creates a new Server.
func New(cfg *config.Config, db *gorm.DB, feFS fs.FS) *Server {
	return &Server{cfg: cfg, db: db, feFS: feFS}
}

// Mumble exposes the in-process Core so the composition root can wire
// Core-owned capabilities (e.g. the remote edge server) to its registry.
func (s *Server) Mumble() *mumble.Server { return s.ms }

// ClientRuntime exposes the shared client transport for shutdown wiring.
func (s *Server) ClientRuntime() *edge.ClientRuntime { return s.runtime }

// Prepare loads persisted configuration, provisions the TLS identity, binds
// the client sockets and constructs the Core + Local Edge + client runtime
// graph. It must complete before Run; capabilities that need the Core's
// registry (RemoteEdgeServer in core mode) are injected between the two.
func (s *Server) Prepare(ctx context.Context) error {
	if err := config.EnsureMetaConfig(s.db, s.cfg); err != nil {
		return fmt.Errorf("ensure meta config: %w", err)
	}
	meta, err := config.LoadMetaConfig(s.db)
	if err != nil {
		return fmt.Errorf("load meta config: %w", err)
	}
	serverCfg, err := config.LoadServerConfig(s.db, 1)
	if err != nil {
		return fmt.Errorf("load server config: %w", err)
	}
	cfg := config.ConfigForServer(meta, serverCfg, s.cfg)
	if err := database.EnsureDefaultVirtualServer(s.db, "Default", cfg.Host, cfg.MumblePort, cfg.MaxUsers, cfg.WelcomeText); err != nil {
		return fmt.Errorf("ensure default virtual server: %w", err)
	}
	if err := config.EnsureServerConfig(s.db, 1, cfg); err != nil {
		return fmt.Errorf("ensure server config: %w", err)
	}

	var certPEM, keyPEM []byte
	if s.cfg.SSLCertPath != "" && s.cfg.SSLKeyPath != "" {
		certPEM, keyPEM, err = transport.LoadOrGenerateCert(s.cfg.SSLCertPath, s.cfg.SSLKeyPath)
		if err != nil {
			return err
		}
	} else {
		certPEM, keyPEM, err = cert.GetOrCreateCertForVirtualServer(s.db, 1)
		if err != nil {
			return fmt.Errorf("load or create cert for virtual server: %w", err)
		}
	}

	ms := mumble.NewServer(cfg, s.db, 1, nil)
	if err := ms.IdentityAuthorityStatus(); err != nil {
		return fmt.Errorf("initialize identity authority: %w", err)
	}
	localEdge := mumble.NewLocalEdge(ms)
	runtime := edge.NewClientRuntime(edge.ClientRuntimeConfig{
		Addr:      formatAddr(cfg.Host, cfg.MumblePort),
		CertPEM:   certPEM,
		KeyPEM:    keyPEM,
		Ingress:   localEdge.Ingress(),
		HandleUDP: localEdge.HandleUDP,
		SetupConn: localEdge.SetupConn,
		OnClose:   localEdge.OnClose,
	})
	if err := runtime.Listen(ctx); err != nil {
		return err
	}
	ms.SetUDPConn(runtime.UDPConn())

	getChanMgr := func(serverID uint) *channel.Manager {
		if serverID == 1 {
			return ms.ChanManager()
		}
		return nil
	}
	onACLChange := func(serverID uint) {
		if serverID == 1 {
			ms.ACLEvaluator().InvalidateCache()
			ms.RefreshSuppressStates()
			ms.RefreshEnterStates()
		}
	}
	onBanChange := func(serverID uint) {
		if serverID == 1 {
			ms.BanManager().Reload()
		}
	}
	onChannelMutated := func(serverID uint, ch interface{}, channelID uint32, removed bool) {
		if serverID != 1 {
			return
		}
		if removed {
			ms.BroadcastChannelRemove(channelID)
		} else if c, ok := ch.(*pkgmumble.Channel); ok {
			ms.BroadcastChannelState(c)
		}
	}
	onConfigChange := func(serverID uint) {
		if serverID != 1 {
			return
		}
		serverCfg, err := config.LoadServerConfig(s.db, 1)
		if err != nil {
			return
		}
		ms.SetVoiceDebug(serverCfg.VoiceDebug)
		ms.SetContentPolicy(serverCfg.AllowRecording, serverCfg.MaxTextMessageLength, serverCfg.MaxImageMessageLength)
	}
	handler := rest.RouterWithMumbleAndIdentity(s.db, cfg, s.feFS, &rest.MumbleUserAdapter{Manager: ms.UserManager(), Server: ms}, &rest.MumbleUserActionAdapter{Server: ms, ServerID: 1}, &rest.MumbleChannelCryptoAdapter{Server: ms, ServerID: 1}, getChanMgr, onACLChange, onBanChange, onChannelMutated, onConfigChange, ms.RevalidateUserNow)

	s.mu.Lock()
	s.ms = ms
	s.runCfg = cfg
	s.runtime = runtime
	s.http = &http.Server{
		Addr:    formatAddr(cfg.Host, cfg.RESTPort),
		Handler: handler,
	}
	s.mu.Unlock()
	slog.Info("REST API listening", "addr", formatAddr(cfg.Host, cfg.RESTPort))
	return nil
}

// Run serves Mumble (control + UDP) and REST connections until ctx is
// cancelled or a loop fails. Prepare must have completed.
func (s *Server) Run(ctx context.Context) error {
	cfg := s.runCfg
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})

	g.Go(func() error {
		return s.runtime.Run(gctx)
	})

	g.Go(func() error {
		return s.ms.StartIdentityRevalidation(gctx)
	})

	if cfg.Bonjour {
		name := cfg.RegisterName
		if name == "" {
			name = "go-mumble-server"
		}
		s.mdns = discovery.NewServer(name, cfg.MumblePort)
		g.Go(func() error {
			if err := s.mdns.Start(gctx); err != nil {
				slog.Warn("mDNS discovery failed (continuing without)", "err", err)
				return nil // non-fatal: server continues without LAN discovery
			}
			return nil
		})
	}

	return g.Wait()
}

func formatAddr(host string, port int) string {
	if host == "" {
		host = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	slog.Info("Server shutting down")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.http != nil {
		s.http.Shutdown(ctx)
	}
	if s.runtime != nil {
		_ = s.runtime.Close()
	}
	if s.mdns != nil {
		s.mdns.Shutdown()
		s.mdns = nil
	}
	return nil
}
