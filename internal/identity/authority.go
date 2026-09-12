// Package identity defines the authority boundary used by Mumble protocol
// authentication, directory lookup, and online revalidation.
package identity

import "context"

type AuthenticateRequest struct {
	ServerInstanceID string `json:"server_instance_id"`
	Username         string `json:"username"`
	Password         string `json:"password"`
	CertificateHash  string `json:"certificate_hash,omitempty"`
	RemoteIP         string `json:"remote_ip,omitempty"`
}

type Identity struct {
	Eligible        bool
	UserID          uint32
	Name            string
	Groups          []string
	IdentityVersion uint64
	PolicyVersion   uint64
	IsSuperUser     bool
}

// Decision keeps an expected business refusal distinct from an unavailable or
// malformed provider response. Provider errors must be handled fail-closed.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
)

type AuthenticateResult struct {
	Decision Decision
	Identity Identity
	Reason   string
}

type ResolveRequest struct {
	UserIDs []uint32 `json:"user_ids,omitempty"`
	Names   []string `json:"names,omitempty"`
}

// Authenticator owns login decisions. IdentityDirectory owns stable ID/name
// lookups. IdentityProvider combines them for a complete identity source.
type Authenticator interface {
	Authenticate(context.Context, AuthenticateRequest) (AuthenticateResult, error)
	External() bool
}

type IdentityDirectory interface {
	Resolve(context.Context, ResolveRequest) ([]Identity, error)
	External() bool
}

type IdentityProvider interface {
	Authenticator
	IdentityDirectory
}

// Authority remains a compatibility alias for the initial implementation.
type Authority = IdentityProvider
