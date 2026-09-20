// Package app is the process composition root: it validates the configured
// runtime mode and constructs exactly the dependency graph that mode is
// allowed to have. The startup invariant is Load config → parse mode →
// validate for mode → build runtime → start; nothing else in the binary
// decides mode-specific shape, and no mode silently substitutes another's
// dependencies.
package app

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/edge"
	"github.com/dchote/go-mumble-server/internal/server"
	"gorm.io/gorm"
)

// Runtime is the mode-specific process lifecycle.
type Runtime interface {
	// Run blocks until ctx is cancelled or a component fails.
	Run(ctx context.Context) error
	// Shutdown gracefully stops every component.
	Shutdown(ctx context.Context) error
}

// App is the composed dependency graph plus its runtime. The exported fields
// are the composition surface: tests assert on which dependencies were
// actually constructed for a mode, never on cfg.Mode alone.
type App struct {
	Mode config.RuntimeMode

	// DB is opened for standalone/core only; edge mode leaves it nil.
	DB *gorm.DB
	// Server is the in-process Core + Local Edge + REST composition
	// (standalone/core); nil in edge mode.
	Server *server.Server
	// RemoteEdge is the core-mode remote edge capability; nil otherwise.
	RemoteEdge *cluster.RemoteEdgeServer
	// EdgeRuntime is the edge-mode edge-local runtime; nil otherwise.
	EdgeRuntime *edge.EdgeRuntime
	// RemoteCore is the edge-mode Core client; nil otherwise.
	RemoteCore edge.RemoteCore
	// ClientRuntime is the shared client transport (all modes).
	ClientRuntime *edge.ClientRuntime

	runtime Runtime
}

// Run starts the composed runtime and blocks until it stops.
func (a *App) Run(ctx context.Context) error { return a.runtime.Run(ctx) }

// Shutdown gracefully stops the composed runtime and releases the database
// handle (standalone/core; a no-op in edge mode).
func (a *App) Shutdown(ctx context.Context) error {
	err := a.runtime.Shutdown(ctx)
	if a.DB != nil {
		if sqlDB, derr := a.DB.DB(); derr == nil {
			_ = sqlDB.Close()
		}
	}
	return err
}

// Build validates the mode's configuration and composes the runtime for it.
func Build(ctx context.Context, cfg *config.Config, feFS fs.FS) (*App, error) {
	if err := config.ValidateForMode(cfg); err != nil {
		return nil, fmt.Errorf("validate %s mode config: %w", cfg.Mode, err)
	}
	if cfg.Mode == config.ModeEdge {
		return buildEdgeRuntime(ctx, cfg)
	}
	return buildLocalRuntime(ctx, cfg, feFS)
}
