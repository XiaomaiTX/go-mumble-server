package cluster

import (
	"context"
	"log/slog"
)

// RemoteEdgeServer is the Core-side capability that will accept remote edge
// registrations over the edge-core protocol. Phase 2A ships the structural
// skeleton only: the core runtime owns and starts the capability, the
// registry integration point is fixed, and the wire protocol, mTLS identity
// and heartbeats are deferred to the network phase — an edge connecting today
// would find nothing listening.
type RemoteEdgeServer struct {
	registry   *Registry
	listenAddr string
}

// NewRemoteEdgeServer binds the capability to the session registry edges will
// register into. listenAddr may be empty, which keeps the capability
// registered but disabled.
func NewRemoteEdgeServer(registry *Registry, listenAddr string) *RemoteEdgeServer {
	return &RemoteEdgeServer{registry: registry, listenAddr: listenAddr}
}

// Registry exposes the registry the edge registrations will bind sessions in.
func (s *RemoteEdgeServer) Registry() *Registry { return s.registry }

// ListenAddr reports the configured (future) edge listener address.
func (s *RemoteEdgeServer) ListenAddr() string { return s.listenAddr }

// Start activates the capability. The network listener itself is implemented
// with the edge-core protocol; today the capability only confirms its
// registration so the core runtime's dependency graph is complete.
func (s *RemoteEdgeServer) Start(context.Context) error {
	if s.listenAddr == "" {
		slog.Info("remote edge server capability registered; edge listener disabled (edge-core protocol pending)")
		return nil
	}
	slog.Info("remote edge server capability registered; edge listener pending edge-core protocol", "addr", s.listenAddr)
	return nil
}

// Close is a no-op while the transport is a skeleton.
func (s *RemoteEdgeServer) Close() error { return nil }
