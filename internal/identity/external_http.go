package identity

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const maxResponseBytes = 1 << 20

var (
	ErrDenied               = errors.New("identity denied")
	ErrAuthorityUnavailable = errors.New("identity authority unavailable")
)

type ExternalHTTPConfig struct {
	BaseURL        string
	ServiceToken   string
	Timeout        time.Duration
	CACertPath     string
	ClientCertPath string
	ClientKeyPath  string
}

type ExternalHTTPAuthority struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewExternalHTTPAuthority(cfg ExternalHTTPConfig) (*ExternalHTTPAuthority, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" || strings.TrimSpace(cfg.ServiceToken) == "" {
		return nil, errors.New("external identity URL and service token are required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 1500 * time.Millisecond
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig, err := externalTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	transport.TLSClientConfig = tlsConfig
	return &ExternalHTTPAuthority{
		baseURL: baseURL,
		token:   strings.TrimSpace(cfg.ServiceToken),
		client:  &http.Client{Timeout: cfg.Timeout, Transport: transport},
	}, nil
}

func externalTLSConfig(cfg ExternalHTTPConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CACertPath != "" {
		pem, err := os.ReadFile(cfg.CACertPath)
		if err != nil {
			return nil, fmt.Errorf("read external auth CA: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("external auth CA contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	if (cfg.ClientCertPath == "") != (cfg.ClientKeyPath == "") {
		return nil, errors.New("external auth client certificate and key must be configured together")
	}
	if cfg.ClientCertPath != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCertPath, cfg.ClientKeyPath)
		if err != nil {
			return nil, fmt.Errorf("load external auth client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	return tlsConfig, nil
}

func (a *ExternalHTTPAuthority) External() bool { return true }

func (a *ExternalHTTPAuthority) Authenticate(ctx context.Context, req AuthenticateRequest) (AuthenticateResult, error) {
	var response struct {
		Decision        string   `json:"decision"`
		UserID          uint32   `json:"user_id"`
		Name            string   `json:"name"`
		Groups          []string `json:"groups"`
		IdentityVersion uint64   `json:"identity_version"`
		PolicyVersion   uint64   `json:"policy_version"`
	}
	if err := a.post(ctx, "/internal/mumble/v1/authenticate", req, &response); err != nil {
		return AuthenticateResult{}, err
	}
	if strings.EqualFold(response.Decision, string(DecisionDeny)) {
		return AuthenticateResult{Decision: DecisionDeny}, nil
	}
	if !strings.EqualFold(response.Decision, string(DecisionAllow)) {
		return AuthenticateResult{}, fmt.Errorf("%w: invalid authentication decision", ErrAuthorityUnavailable)
	}
	identity := Identity{Eligible: true, UserID: response.UserID, Name: response.Name, Groups: response.Groups, IdentityVersion: response.IdentityVersion, PolicyVersion: response.PolicyVersion}
	if err := validateExternalIdentity(identity); err != nil {
		return AuthenticateResult{}, fmt.Errorf("invalid external identity response: %w", err)
	}
	return AuthenticateResult{Decision: DecisionAllow, Identity: identity}, nil
}

func (a *ExternalHTTPAuthority) Resolve(ctx context.Context, req ResolveRequest) ([]Identity, error) {
	var response struct {
		Identities []struct {
			Eligible        bool     `json:"eligible"`
			UserID          uint32   `json:"user_id"`
			Name            string   `json:"name"`
			Groups          []string `json:"groups"`
			IdentityVersion uint64   `json:"identity_version"`
			PolicyVersion   uint64   `json:"policy_version"`
		} `json:"identities"`
	}
	if err := a.post(ctx, "/internal/mumble/v1/identities/resolve", req, &response); err != nil {
		return nil, err
	}
	identities := make([]Identity, 0, len(response.Identities))
	for _, item := range response.Identities {
		identity := Identity{Eligible: item.Eligible, UserID: item.UserID, Name: item.Name, Groups: item.Groups, IdentityVersion: item.IdentityVersion, PolicyVersion: item.PolicyVersion}
		if identity.Eligible {
			if err := validateExternalIdentity(identity); err != nil {
				return nil, fmt.Errorf("invalid external identity response: %w", err)
			}
		}
		identities = append(identities, identity)
	}
	return identities, nil
}

func validateExternalIdentity(identity Identity) error {
	if identity.UserID == 0 {
		return errors.New("user id 0 is reserved")
	}
	if strings.TrimSpace(identity.Name) == "" || identity.Name == "SuperUser" {
		return errors.New("invalid or reserved canonical name")
	}
	seen := make(map[string]struct{}, len(identity.Groups))
	for _, group := range identity.Groups {
		if strings.TrimSpace(group) == "" || strings.TrimSpace(group) != group {
			return errors.New("external group is empty or not canonical")
		}
		if _, duplicate := seen[group]; duplicate {
			return errors.New("duplicate external group")
		}
		seen[group] = struct{}{}
	}
	return nil
}

func (a *ExternalHTTPAuthority) post(ctx context.Context, path string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAuthorityUnavailable, err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxResponseBytes)
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, limited)
		return fmt.Errorf("%w: HTTP %d", ErrAuthorityUnavailable, resp.StatusCode)
	}
	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(limited).Decode(&envelope); err != nil {
		return fmt.Errorf("%w: decode response", ErrAuthorityUnavailable)
	}
	if envelope.Code != 0 && envelope.Code != 200 {
		return fmt.Errorf("%w: authority code %d", ErrAuthorityUnavailable, envelope.Code)
	}
	if err := json.Unmarshal(envelope.Data, output); err != nil {
		return fmt.Errorf("%w: decode response data", ErrAuthorityUnavailable)
	}
	return nil
}
