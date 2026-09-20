package app

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/edge"
)

// Composition tests assert on the dependency graph that was actually
// constructed for a mode — never on cfg.Mode alone.

// freePort borrows an ephemeral port from the OS. The persisted meta config's
// column defaults override a zero port, so tests must configure real ports.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("borrow port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func modeConfig(t *testing.T, mode config.RuntimeMode) *config.Config {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Mode = mode
	cfg.Host = "127.0.0.1"
	cfg.MumblePort = freePort(t)
	cfg.RESTPort = freePort(t)
	cfg.Bonjour = false
	cfg.DatabasePath = filepath.Join(t.TempDir(), "app-test.sqlite")
	cfg.CoreAddress = "127.0.0.1:64740"
	cfg.EdgeID = "app-test-edge"
	return cfg
}

func build(t *testing.T, mode config.RuntimeMode) *App {
	t.Helper()
	a, err := Build(context.Background(), modeConfig(t, mode), nil)
	if err != nil {
		t.Fatalf("Build(%s): %v", mode, err)
	}
	t.Cleanup(func() { _ = a.Shutdown(context.Background()) })
	return a
}

// standalone = Core + Local Edge in one process: database and the full Core
// graph exist, and none of the distributed capabilities do.
func TestBuildStandaloneComposesCoreWithLocalEdge(t *testing.T) {
	a := build(t, config.ModeStandalone)

	if a.DB == nil {
		t.Fatal("standalone did not open the database")
	}
	if a.Server == nil {
		t.Fatal("standalone did not build the in-process Core server")
	}
	if a.ClientRuntime == nil {
		t.Fatal("standalone did not build the shared client runtime")
	}
	if a.RemoteEdge != nil {
		t.Fatal("standalone must not construct the remote edge capability")
	}
	if a.EdgeRuntime != nil || a.RemoteCore != nil {
		t.Fatal("standalone must not construct edge-mode runtimes")
	}
	ms := a.Server.Mumble()
	if ms == nil {
		t.Fatal("in-process Core missing")
	}
	if !ms.Registry().EdgeRegistered(cluster.LocalEdgeID) {
		t.Fatal("local edge not registered in the session registry")
	}
	if ms.Dispatcher().VoiceTransportFor(cluster.LocalEdgeID) == nil {
		t.Fatal("local voice transport not registered with the dispatcher")
	}
	if ms.UserManager() == nil || ms.ChanManager() == nil || ms.ACLEvaluator() == nil || ms.BanManager() == nil {
		t.Fatal("standalone Core is missing authoritative managers")
	}
	if a.Server.ClientRuntime() != a.ClientRuntime {
		t.Fatal("App.ClientRuntime is not the server's shared client runtime")
	}
}

// core = the standalone graph plus the remote edge capability, bound to the
// real session registry between Prepare and Run.
func TestBuildCoreAddsRemoteEdgeCapability(t *testing.T) {
	a := build(t, config.ModeCore)

	if a.DB == nil || a.Server == nil || a.ClientRuntime == nil {
		t.Fatal("core mode lost the standalone graph")
	}
	if a.EdgeRuntime != nil || a.RemoteCore != nil {
		t.Fatal("core mode must not construct edge-mode runtimes")
	}
	if a.RemoteEdge == nil {
		t.Fatal("core mode did not construct the remote edge capability")
	}
	if a.RemoteEdge.Registry() != a.Server.Mumble().Registry() {
		t.Fatal("remote edge capability is not bound to the real session registry")
	}
	if !a.Server.Mumble().Registry().EdgeRegistered(cluster.LocalEdgeID) {
		t.Fatal("core mode dropped the local edge registration")
	}
}

// standalone and core compose the identical Core + Local Edge graph; the
// remote edge capability is the only addition core mode makes.
func TestStandaloneAndCoreAreEquivalent(t *testing.T) {
	standalone := build(t, config.ModeStandalone)
	core := build(t, config.ModeCore)

	for name, a := range map[string]*App{"standalone": standalone, "core": core} {
		ms := a.Server.Mumble()
		if !ms.Registry().EdgeRegistered(cluster.LocalEdgeID) {
			t.Errorf("%s: local edge not registered", name)
		}
		if ms.Dispatcher().VoiceTransportFor(cluster.LocalEdgeID) == nil {
			t.Errorf("%s: local voice transport missing", name)
		}
		if ms.UserManager() == nil || ms.ChanManager() == nil || ms.ACLEvaluator() == nil || ms.BanManager() == nil {
			t.Errorf("%s: authoritative managers missing", name)
		}
		if len(ms.HandlerTable()) == 0 {
			t.Errorf("%s: empty handler table", name)
		}
	}
	if standalone.Server.Mumble().Registry() == core.Server.Mumble().Registry() {
		t.Fatal("the two compositions must not share a registry")
	}
}

// edge = transport-only process: no database is opened, no in-process Core
// exists, and the remote core client fails closed.
func TestBuildEdgeComposesTransportWithoutCoreState(t *testing.T) {
	a := build(t, config.ModeEdge)

	if a.DB != nil {
		t.Fatal("edge mode opened the database")
	}
	if a.Server != nil {
		t.Fatal("edge mode constructed the in-process Core server")
	}
	if a.RemoteEdge != nil {
		t.Fatal("edge mode must not construct the remote edge capability")
	}
	if a.EdgeRuntime == nil {
		t.Fatal("edge mode did not build the edge runtime")
	}
	if a.RemoteCore == nil {
		t.Fatal("edge mode did not build the remote core client")
	}
	if a.ClientRuntime == nil {
		t.Fatal("edge mode did not build the shared client runtime")
	}
	if _, err := a.RemoteCore.Authenticate(context.Background(), edge.AuthForward{Username: "bob"}); !errors.Is(err, edge.ErrCoreProtocolNotImplemented) {
		t.Fatalf("remote core Authenticate err = %v, want ErrCoreProtocolNotImplemented", err)
	}
}

// A missing Core address is a configuration error — the edge never falls
// back to substituting a local Core.
func TestBuildEdgeRequiresCoreAddress(t *testing.T) {
	cfg := modeConfig(t, config.ModeEdge)
	cfg.CoreAddress = ""
	if _, err := Build(context.Background(), cfg, nil); err == nil {
		t.Fatal("Build accepted edge mode without a core address")
	}
}

func TestBuildRejectsInvalidModeConfig(t *testing.T) {
	cfg := modeConfig(t, config.ModeStandalone)
	cfg.DatabasePath = ""
	if _, err := Build(context.Background(), cfg, nil); err == nil {
		t.Fatal("Build accepted standalone mode without a database path")
	}
}
