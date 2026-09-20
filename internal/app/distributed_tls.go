package app

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"

	"github.com/dchote/go-mumble-server/internal/config"
)

func coreEdgeTLS(cfg *config.Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.EdgeTLSCertPath, cfg.EdgeTLSKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load edge server certificate: %w", err)
	}
	pem, err := os.ReadFile(cfg.EdgeClientCAPath)
	if err != nil {
		return nil, fmt.Errorf("read edge client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("edge client CA contains no certificates")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}, nil
}

func edgeCoreTLS(cfg *config.Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.EdgeClientCertPath, cfg.EdgeClientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load edge client certificate: %w", err)
	}
	pem, err := os.ReadFile(cfg.CoreCACertPath)
	if err != nil {
		return nil, fmt.Errorf("read Core CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("Core CA contains no certificates")
	}
	name := cfg.CoreServerName
	if name == "" {
		name, _, _ = net.SplitHostPort(cfg.CoreAddress)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: name}, nil
}
