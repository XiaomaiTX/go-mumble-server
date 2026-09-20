package app

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/database"
	"github.com/dchote/go-mumble-server/internal/server"
)

// buildLocalRuntime composes standalone and core: one process hosting the
// authoritative Core and a Local Edge over the shared client transport. The
// only difference between the two is the core-mode RemoteEdgeServer
// capability, injected between Prepare and Run so it binds the real session
// registry rather than a placeholder.
func buildLocalRuntime(ctx context.Context, cfg *config.Config, feFS fs.FS) (*App, error) {
	db, err := database.Open(cfg.DatabasePath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	srv := server.New(cfg, db, feFS)
	if err := srv.Prepare(ctx); err != nil {
		return nil, err
	}
	a := &App{
		Mode:          cfg.Mode,
		DB:            db,
		Server:        srv,
		ClientRuntime: srv.ClientRuntime(),
	}
	if cfg.Mode == config.ModeCore {
		a.RemoteEdge = cluster.NewRemoteEdgeServer(srv.Mumble().Registry(), cfg.EdgeListenAddr)
		a.runtime = &coreRuntime{srv: srv, remoteEdge: a.RemoteEdge}
		return a, nil
	}
	a.runtime = srv
	return a, nil
}

// coreRuntime runs the core process: the standalone graph plus the remote
// edge capability bound to the Core's session registry.
type coreRuntime struct {
	srv        *server.Server
	remoteEdge *cluster.RemoteEdgeServer
}

func (c *coreRuntime) Run(ctx context.Context) error {
	if err := c.remoteEdge.Start(ctx); err != nil {
		return fmt.Errorf("start remote edge server: %w", err)
	}
	defer c.remoteEdge.Close()
	return c.srv.Run(ctx)
}

func (c *coreRuntime) Shutdown(ctx context.Context) error {
	_ = c.remoteEdge.Close()
	return c.srv.Shutdown(ctx)
}
