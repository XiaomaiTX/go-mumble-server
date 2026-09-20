package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if cfg.MumblePort != 64738 {
		t.Errorf("MumblePort = %d, want 64738", cfg.MumblePort)
	}
	if cfg.RESTPort != 64730 {
		t.Errorf("RESTPort = %d, want 64730", cfg.RESTPort)
	}
	if cfg.DatabasePath != "mumble-server.sqlite" {
		t.Errorf("DatabasePath = %q, want mumble-server.sqlite", cfg.DatabasePath)
	}
}

func TestLoad_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mumble-server.toml")
	toml := `
[network]
port = 64739
rest_port = 9091
host = "127.0.0.1"

[database]
path = "/var/lib/mumble/db.sqlite"

[logging]
level = "debug"

[auth]
mode = "external"
external_url = "https://identity.example"
authenticate_path = "/provider/login"
resolve_path = "/provider/directory"
`
	if err := os.WriteFile(path, []byte(toml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MumblePort != 64739 {
		t.Errorf("MumblePort = %d, want 64739", cfg.MumblePort)
	}
	if cfg.RESTPort != 9091 {
		t.Errorf("RESTPort = %d, want 9091", cfg.RESTPort)
	}
	if cfg.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want 127.0.0.1", cfg.Host)
	}
	if cfg.DatabasePath != "/var/lib/mumble/db.sqlite" {
		t.Errorf("DatabasePath = %q, want /var/lib/mumble/db.sqlite", cfg.DatabasePath)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.ExternalAuthAuthenticatePath != "/provider/login" || cfg.ExternalAuthResolvePath != "/provider/directory" {
		t.Errorf("external paths = %q, %q", cfg.ExternalAuthAuthenticatePath, cfg.ExternalAuthResolvePath)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mumble-server.toml")
	if err := os.WriteFile(path, []byte("[network]\nport = 64738\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	os.Setenv("MUMBLE_MUMBLE_PORT", "64740")
	defer os.Unsetenv("MUMBLE_MUMBLE_PORT")
	os.Setenv("MUMBLE_LOG_LEVEL", "warn")
	defer os.Unsetenv("MUMBLE_LOG_LEVEL")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MumblePort != 64740 {
		t.Errorf("MumblePort = %d, want 64740 (from env)", cfg.MumblePort)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want warn (from env)", cfg.LogLevel)
	}
}

func TestLoad_NonexistentFile(t *testing.T) {
	_, err := Load("/nonexistent/mumble-server.toml")
	if err == nil {
		t.Fatal("Load expected error for nonexistent file")
	}
}

func TestParseRuntimeMode(t *testing.T) {
	for _, s := range []string{"standalone", "core", "edge"} {
		m, err := ParseRuntimeMode(s)
		if err != nil {
			t.Errorf("ParseRuntimeMode(%q): %v", s, err)
		} else if string(m) != s {
			t.Errorf("ParseRuntimeMode(%q) = %q", s, m)
		}
	}
	if _, err := ParseRuntimeMode("cluster"); err == nil {
		t.Error("ParseRuntimeMode accepted an unknown mode")
	}
}

func TestValidateForMode(t *testing.T) {
	base := func(mutate func(*Config)) *Config {
		t.Helper()
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		cfg.Mode = ModeStandalone
		mutate(cfg)
		return cfg
	}
	cases := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{"standalone defaults", base(func(*Config) {}), ""},
		{"standalone missing database", base(func(c *Config) { c.DatabasePath = "" }), "database path"},
		{"standalone half cert pair", base(func(c *Config) { c.SSLCertPath = "a.crt" }), "together"},
		{"core defaults", base(func(c *Config) { c.Mode = ModeCore }), ""},
		{"core bad edge listen", base(func(c *Config) {
			c.Mode = ModeCore
			c.EdgeListenAddr = "no-port"
		}), "host:port"},
		{"core valid edge listen", base(func(c *Config) {
			c.Mode = ModeCore
			c.EdgeListenAddr = "127.0.0.1:64740"
		}), ""},
		{"edge missing core address", base(func(c *Config) { c.Mode = ModeEdge }), "core_address"},
		{"edge bad core address", base(func(c *Config) {
			c.Mode = ModeEdge
			c.CoreAddress = "no-port"
			c.EdgeID = "edge-1"
		}), "host:port"},
		{"edge missing edge id", base(func(c *Config) {
			c.Mode = ModeEdge
			c.CoreAddress = "127.0.0.1:64740"
		}), "edge_id"},
		{"edge valid", base(func(c *Config) {
			c.Mode = ModeEdge
			c.CoreAddress = "127.0.0.1:64740"
			c.EdgeID = "edge-1"
		}), ""},
		{"edge unreadable core ca", base(func(c *Config) {
			c.Mode = ModeEdge
			c.CoreAddress = "127.0.0.1:64740"
			c.EdgeID = "edge-1"
			c.CoreCACertPath = filepath.Join(t.TempDir(), "missing-ca.crt")
		}), "core_ca_cert"},
		{"unknown mode", base(func(c *Config) { c.Mode = RuntimeMode("bogus") }), "unsupported runtime mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateForMode(tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateForMode: unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateForMode error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
