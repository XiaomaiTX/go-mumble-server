package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dchote/go-mumble-server/internal/app"
	"github.com/dchote/go-mumble-server/internal/config"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("go-mumble-server %s (commit: %s, built: %s)\n", version, commit, buildTime)
		os.Exit(0)
	}

	configPath := flag.String("config", "", "Path to mumble-server.toml")
	frontendEmbed := flag.Bool("frontend-embed", true, "Serve embedded web UI on REST port")
	mode := flag.String("mode", "", "Runtime mode: standalone, core or edge")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	cfg.FrontendEmbed = *frontendEmbed
	if *mode != "" {
		parsed, parseErr := config.ParseRuntimeMode(*mode)
		if parseErr != nil {
			slog.Error("config validation failed", "err", parseErr)
			os.Exit(1)
		}
		cfg.Mode = parsed
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	var feFS fs.FS
	if cfg.FrontendEmbed {
		feFS, err = getFrontendFS()
		if err != nil {
			slog.Warn("frontend embed unavailable, SPA routes will 404", "err", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	application, err := app.Build(ctx, cfg, feFS)
	if err != nil {
		slog.Error("runtime composition failed", "mode", cfg.Mode, "err", err)
		os.Exit(1)
	}

	done := make(chan error, 1)
	go func() {
		done <- application.Run(ctx)
	}()

	select {
	case <-waitForShutdown():
	case err = <-done:
		if err != nil {
			slog.Error("server exit", "err", err)
		}
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = application.Shutdown(shutdownCtx)
	select {
	case err = <-done:
		if err != nil {
			slog.Error("server exit", "err", err)
		}
	default:
	}
}

func waitForShutdown() <-chan struct{} {
	done := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		close(done)
	}()
	return done
}
