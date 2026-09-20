package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type RuntimeMode string

const (
	ModeStandalone RuntimeMode = "standalone"
	ModeCore       RuntimeMode = "core"
)

func ParseRuntimeMode(value string) (RuntimeMode, error) {
	mode := RuntimeMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case ModeStandalone, ModeCore:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid distributed mode %q (expected standalone or core)", value)
	}
}

// Config holds the server configuration.
type Config struct {
	Mode           RuntimeMode
	Host           string
	MumblePort     int
	RESTPort       int
	FrontendEmbed  bool
	DatabasePath   string
	SSLCertPath    string
	SSLKeyPath     string
	MaxUsers       int
	MaxBandwidth   int
	LogLevel       string
	JWTIssuer      string
	JWTAudience    string
	JWTExpiryDays  int
	ChannelDepth   int
	ChannelCount   int
	WelcomeText    string
	ServerPassword string
	DefaultChannel int
	CertRequired   bool
	Bonjour        bool
	RegisterName   string
	VoiceDebug     bool
	// AllowRecording mirrors murmur's allowRecording: when false a client that
	// announces it started recording is disconnected instead of being relayed.
	AllowRecording bool
	// MaxTextMessageLength and MaxImageMessageLength bound user-supplied text and
	// image payloads (chat messages, user comments, avatar textures), matching
	// murmur's iMaxTextMessageLength / iMaxImageMessageLength. 0 means unlimited.
	MaxTextMessageLength  int
	MaxImageMessageLength int
	// MaxChannelListeners and MaxListenersPerUser cap Mumble 1.4+ channel listening,
	// matching murmur's iMaxListenersPerChannel / iMaxListenerProxiesPerUser.
	// 0 means unlimited.
	MaxChannelListeners          int
	MaxListenersPerUser          int
	AuthMode                     string
	ExternalAuthURL              string
	ExternalAuthAuthenticatePath string
	ExternalAuthResolvePath      string
	ExternalAuthServiceToken     string
	ExternalAuthServerInstanceID string
	ExternalAuthTimeout          time.Duration
	ExternalAuthRevalidate       time.Duration
	ExternalAuthStaleGrace       time.Duration
	ExternalAuthCACertPath       string
	ExternalAuthClientCertPath   string
	ExternalAuthClientKeyPath    string
	IdentityRevalidateToken      string
}

// fileConfig mirrors the TOML structure for parsing.
type fileConfig struct {
	Distributed struct {
		Mode string `toml:"mode"`
	} `toml:"distributed"`
	Network struct {
		Port     int    `toml:"port"`
		RestPort int    `toml:"rest_port"`
		Host     string `toml:"host"`
	} `toml:"network"`
	TLS struct {
		Cert string `toml:"cert"`
		Key  string `toml:"key"`
	} `toml:"tls"`
	Server struct {
		MaxUsers               int    `toml:"max_users"`
		MaxBandwidth           int    `toml:"max_bandwidth"`
		WelcomeText            string `toml:"welcome_text"`
		ServerPassword         string `toml:"server_password"`
		MaxListenersPerChannel int    `toml:"max_listeners_per_channel"`
		MaxListenersPerUser    int    `toml:"max_listeners_per_user"`
	} `toml:"server"`
	Database struct {
		Path string `toml:"path"`
	} `toml:"database"`
	Logging struct {
		Level string `toml:"level"`
	} `toml:"logging"`
	Channels struct {
		NestingLimit int `toml:"nesting_limit"`
		CountLimit   int `toml:"count_limit"`
	} `toml:"channels"`
	Users struct {
		DefaultChannel int  `toml:"default_channel"`
		CertRequired   bool `toml:"cert_required"`
	} `toml:"users"`
	Bonjour struct {
		Enabled bool   `toml:"enabled"`
		Name    string `toml:"register_name"`
	} `toml:"bonjour"`
	Auth struct {
		Mode                      string `toml:"mode"`
		ExternalURL               string `toml:"external_url"`
		AuthenticatePath          string `toml:"authenticate_path"`
		ResolvePath               string `toml:"resolve_path"`
		ServiceToken              string `toml:"service_token"`
		ServerInstanceID          string `toml:"server_instance_id"`
		TimeoutMS                 int    `toml:"timeout_ms"`
		RevalidateIntervalSeconds int    `toml:"revalidate_interval_seconds"`
		StaleGraceSeconds         int    `toml:"stale_grace_seconds"`
		CACert                    string `toml:"ca_cert"`
		ClientCert                string `toml:"client_cert"`
		ClientKey                 string `toml:"client_key"`
		IdentityRevalidateToken   string `toml:"identity_revalidate_token"`
	} `toml:"auth"`
}

// defaults returns the default configuration.
func defaults() *Config {
	return &Config{
		Mode:          ModeStandalone,
		Host:          "0.0.0.0",
		MumblePort:    64738,
		RESTPort:      64730,
		FrontendEmbed: true,
		DatabasePath:  "mumble-server.sqlite",
		LogLevel:      "info",
		JWTIssuer:     "go-mumble-server",
		JWTAudience:   "go-mumble-server-api",
		JWTExpiryDays: 30,
		MaxUsers:      100,
		MaxBandwidth:  72000,
		ChannelDepth:  10,
		ChannelCount:  1000,
		// Murmur's defaults for the same settings.
		AllowRecording:               true,
		MaxTextMessageLength:         5000,
		MaxImageMessageLength:        131072,
		MaxChannelListeners:          0, // unlimited, like murmur's -1
		MaxListenersPerUser:          0,
		AuthMode:                     "local",
		ExternalAuthAuthenticatePath: "/api/internal/mumble/v1/authenticate",
		ExternalAuthResolvePath:      "/api/internal/mumble/v1/identities/resolve",
		ExternalAuthTimeout:          1500 * time.Millisecond,
		ExternalAuthRevalidate:       45 * time.Second,
		ExternalAuthStaleGrace:       3 * time.Minute,
	}
}

// Load reads configuration with precedence: file (if path set) < env (MUMBLE_*) < defaults.
// Flags (e.g. -frontend-embed) are applied by the caller after Load.
func Load(path string) (*Config, error) {
	cfg := defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		var fc fileConfig
		if err := toml.Unmarshal(data, &fc); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		applyFileConfig(cfg, &fc)
	}

	applyEnv(cfg)
	if _, err := ParseRuntimeMode(string(cfg.Mode)); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyFileConfig(cfg *Config, fc *fileConfig) {
	if fc.Distributed.Mode != "" {
		cfg.Mode = RuntimeMode(strings.ToLower(strings.TrimSpace(fc.Distributed.Mode)))
	}
	if fc.Network.Host != "" {
		cfg.Host = fc.Network.Host
	}
	if fc.Network.Port != 0 {
		cfg.MumblePort = fc.Network.Port
	}
	if fc.Network.RestPort != 0 {
		cfg.RESTPort = fc.Network.RestPort
	}
	if fc.TLS.Cert != "" {
		cfg.SSLCertPath = fc.TLS.Cert
	}
	if fc.TLS.Key != "" {
		cfg.SSLKeyPath = fc.TLS.Key
	}
	if fc.Server.MaxUsers != 0 {
		cfg.MaxUsers = fc.Server.MaxUsers
	}
	if fc.Server.MaxBandwidth != 0 {
		cfg.MaxBandwidth = fc.Server.MaxBandwidth
	}
	if fc.Server.MaxListenersPerChannel > 0 {
		cfg.MaxChannelListeners = fc.Server.MaxListenersPerChannel
	}
	if fc.Server.MaxListenersPerUser > 0 {
		cfg.MaxListenersPerUser = fc.Server.MaxListenersPerUser
	}
	cfg.WelcomeText = fc.Server.WelcomeText
	cfg.ServerPassword = fc.Server.ServerPassword
	if fc.Database.Path != "" {
		cfg.DatabasePath = fc.Database.Path
	}
	if fc.Logging.Level != "" {
		cfg.LogLevel = fc.Logging.Level
	}
	if fc.Channels.NestingLimit != 0 {
		cfg.ChannelDepth = fc.Channels.NestingLimit
	}
	if fc.Channels.CountLimit != 0 {
		cfg.ChannelCount = fc.Channels.CountLimit
	}
	cfg.DefaultChannel = fc.Users.DefaultChannel
	cfg.CertRequired = fc.Users.CertRequired
	if fc.Bonjour.Enabled {
		cfg.Bonjour = true
	}
	if fc.Bonjour.Name != "" {
		cfg.RegisterName = fc.Bonjour.Name
	}
	if fc.Auth.Mode != "" {
		cfg.AuthMode = strings.ToLower(fc.Auth.Mode)
	}
	cfg.ExternalAuthURL = fc.Auth.ExternalURL
	if fc.Auth.AuthenticatePath != "" {
		cfg.ExternalAuthAuthenticatePath = fc.Auth.AuthenticatePath
	}
	if fc.Auth.ResolvePath != "" {
		cfg.ExternalAuthResolvePath = fc.Auth.ResolvePath
	}
	cfg.ExternalAuthServiceToken = fc.Auth.ServiceToken
	cfg.ExternalAuthServerInstanceID = fc.Auth.ServerInstanceID
	if fc.Auth.TimeoutMS > 0 {
		cfg.ExternalAuthTimeout = time.Duration(fc.Auth.TimeoutMS) * time.Millisecond
	}
	if fc.Auth.RevalidateIntervalSeconds > 0 {
		cfg.ExternalAuthRevalidate = time.Duration(fc.Auth.RevalidateIntervalSeconds) * time.Second
	}
	if fc.Auth.StaleGraceSeconds > 0 {
		cfg.ExternalAuthStaleGrace = time.Duration(fc.Auth.StaleGraceSeconds) * time.Second
	}
	cfg.ExternalAuthCACertPath = fc.Auth.CACert
	cfg.ExternalAuthClientCertPath = fc.Auth.ClientCert
	cfg.ExternalAuthClientKeyPath = fc.Auth.ClientKey
	cfg.IdentityRevalidateToken = fc.Auth.IdentityRevalidateToken
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("MUMBLE_MODE"); v != "" {
		cfg.Mode = RuntimeMode(strings.ToLower(strings.TrimSpace(v)))
	}
	if v := os.Getenv("MUMBLE_HOST"); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv("MUMBLE_MUMBLE_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MumblePort = n
		}
	}
	if v := os.Getenv("MUMBLE_REST_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RESTPort = n
		}
	}
	if v := os.Getenv("MUMBLE_DATABASE_PATH"); v != "" {
		cfg.DatabasePath = v
	}
	if v := os.Getenv("MUMBLE_SSL_CERT_PATH"); v != "" {
		cfg.SSLCertPath = v
	}
	if v := os.Getenv("MUMBLE_SSL_KEY_PATH"); v != "" {
		cfg.SSLKeyPath = v
	}
	if v := os.Getenv("MUMBLE_MAX_USERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxUsers = n
		}
	}
	if v := os.Getenv("MUMBLE_MAX_BANDWIDTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxBandwidth = n
		}
	}
	if v := os.Getenv("MUMBLE_MAX_LISTENERS_PER_CHANNEL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxChannelListeners = n
		}
	}
	if v := os.Getenv("MUMBLE_MAX_LISTENERS_PER_USER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxListenersPerUser = n
		}
	}
	if v := os.Getenv("MUMBLE_LOG_LEVEL"); v != "" {
		cfg.LogLevel = strings.ToLower(v)
	}
	if v := os.Getenv("MUMBLE_JWT_ISSUER"); v != "" {
		cfg.JWTIssuer = v
	}
	if v := os.Getenv("MUMBLE_JWT_AUDIENCE"); v != "" {
		cfg.JWTAudience = v
	}
	if v := os.Getenv("MUMBLE_JWT_EXPIRY_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.JWTExpiryDays = n
		}
	}
	if v := os.Getenv("MUMBLE_BONJOUR"); v != "" {
		cfg.Bonjour = strings.ToLower(v) == "true" || v == "1"
	}
	if v := os.Getenv("MUMBLE_REGISTER_NAME"); v != "" {
		cfg.RegisterName = v
	}
	if v := os.Getenv("MUMBLE_WELCOME_TEXT"); v != "" {
		cfg.WelcomeText = v
	}
	if v := os.Getenv("MUMBLE_SERVER_PASSWORD"); v != "" {
		cfg.ServerPassword = v
	}
	if v := os.Getenv("MUMBLE_CHANNEL_NESTING_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ChannelDepth = n
		}
	}
	if v := os.Getenv("MUMBLE_CHANNEL_COUNT_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ChannelCount = n
		}
	}
	if v := os.Getenv("MUMBLE_DEFAULT_CHANNEL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.DefaultChannel = n
		}
	}
	if v := os.Getenv("MUMBLE_CERT_REQUIRED"); v != "" {
		cfg.CertRequired = strings.ToLower(v) == "true" || v == "1"
	}
	if v := os.Getenv("MUMBLE_AUTH_MODE"); v != "" {
		cfg.AuthMode = strings.ToLower(v)
	}
	if v := os.Getenv("MUMBLE_EXTERNAL_AUTH_URL"); v != "" {
		cfg.ExternalAuthURL = v
	}
	if v := os.Getenv("MUMBLE_EXTERNAL_AUTH_AUTHENTICATE_PATH"); v != "" {
		cfg.ExternalAuthAuthenticatePath = v
	}
	if v := os.Getenv("MUMBLE_EXTERNAL_AUTH_RESOLVE_PATH"); v != "" {
		cfg.ExternalAuthResolvePath = v
	}
	if v := os.Getenv("MUMBLE_EXTERNAL_AUTH_SERVICE_TOKEN"); v != "" {
		cfg.ExternalAuthServiceToken = v
	}
	if v := os.Getenv("MUMBLE_EXTERNAL_AUTH_SERVER_INSTANCE_ID"); v != "" {
		cfg.ExternalAuthServerInstanceID = v
	}
	if v := os.Getenv("MUMBLE_EXTERNAL_AUTH_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ExternalAuthTimeout = time.Duration(n) * time.Millisecond
		}
	}
	if v := os.Getenv("MUMBLE_EXTERNAL_AUTH_REVALIDATE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ExternalAuthRevalidate = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("MUMBLE_EXTERNAL_AUTH_STALE_GRACE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ExternalAuthStaleGrace = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("MUMBLE_EXTERNAL_AUTH_CA_CERT"); v != "" {
		cfg.ExternalAuthCACertPath = v
	}
	if v := os.Getenv("MUMBLE_EXTERNAL_AUTH_CLIENT_CERT"); v != "" {
		cfg.ExternalAuthClientCertPath = v
	}
	if v := os.Getenv("MUMBLE_EXTERNAL_AUTH_CLIENT_KEY"); v != "" {
		cfg.ExternalAuthClientKeyPath = v
	}
	if v := os.Getenv("MUMBLE_IDENTITY_REVALIDATE_TOKEN"); v != "" {
		cfg.IdentityRevalidateToken = v
	}
}
