package mumble

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/database/models"
	"github.com/dchote/go-mumble-server/internal/identity"
	pkgmumble "github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

type fakeAuthority struct {
	resolved []identity.Identity
	err      error
}

func (f *fakeAuthority) External() bool { return true }
func (f *fakeAuthority) Authenticate(context.Context, identity.AuthenticateRequest) (identity.AuthenticateResult, error) {
	return identity.AuthenticateResult{}, f.err
}
func (f *fakeAuthority) Resolve(context.Context, identity.ResolveRequest) ([]identity.Identity, error) {
	return f.resolved, f.err
}

func TestExternalModeNeverFallsBackToRegisteredUsers(t *testing.T) {
	seat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":{"decision":"deny","reason_code":"INVALID_CREDENTIALS"}}`))
	}))
	defer seat.Close()
	dbServer := newACLTestServer(t)
	closeTestDBBeforeTempCleanup(t, dbServer)
	if err := dbServer.db.Create(&models.RegisteredUser{ServerID: 1, UserID: 44, Name: "local-user", PasswordHash: "irrelevant"}).Error; err != nil {
		t.Fatal(err)
	}
	srv := NewServer(&config.Config{MaxUsers: 10, AuthMode: "external", ExternalAuthURL: seat.URL, ExternalAuthServiceToken: "token", ExternalAuthTimeout: time.Second}, dbServer.db, 1, nil)
	conn, _ := newAuthenticatingConn(t)
	payload, _ := (&messages.Authenticate{Username: "local-user", Password: "local-password"}).Marshal()
	if err := srv.handleAuthenticate(protocol.MessageAuthenticate, payload, conn); err != nil {
		t.Fatal(err)
	}
	if srv.users.Count() != 0 {
		t.Fatal("external deny must not fall back to registered_users")
	}
}

func TestRevalidationUpdatesRuntimeGroupsAndCanonicalName(t *testing.T) {
	srv := newACLTestServer(t)
	closeTestDBBeforeTempCleanup(t, srv)
	srv.cfg.ExternalAuthStaleGrace = time.Minute
	stored, ok := srv.users.Add(pkgmumble.User{UserID: 173, Name: "Old Name", ExternalIdentity: true, ExternalGroups: []string{"role:leader"}, IdentityLastValidatedAt: time.Now()})
	if !ok {
		t.Fatal("add external user")
	}
	ref := cluster.SessionRef{SessionID: stored.SessionID, Generation: stored.SessionGeneration}
	if err := srv.registry.Bind(ref, cluster.LocalEdgeID); err != nil {
		t.Fatal(err)
	}
	if err := srv.registry.CommitActive(ref); err != nil {
		t.Fatal(err)
	}
	srv.authority = &fakeAuthority{resolved: []identity.Identity{{Eligible: true, UserID: 173, Name: "New Name", Groups: []string{"authenticated", "role:member"}, IdentityVersion: 3, PolicyVersion: 9}}}
	if err := srv.RevalidateUserNow(context.Background(), 173); err != nil {
		t.Fatal(err)
	}
	updated, ok := srv.users.Snapshot(stored.SessionID)
	if !ok || updated.Name != "New Name" || !slices.Equal(updated.ExternalGroups, []string{"authenticated", "role:member"}) {
		t.Fatalf("updated identity = %+v, %v", updated, ok)
	}
}

func TestSanitizedClientTokensOnlyNormalizesInput(t *testing.T) {
	got := sanitizedClientTokens([]string{"ordinary", "  authority-claim  "})
	if !slices.Equal(got, []string{"ordinary", "authority-claim"}) {
		t.Fatalf("sanitized tokens = %v", got)
	}
}

func closeTestDBBeforeTempCleanup(t *testing.T, srv *Server) {
	t.Helper()
	sqlDB, err := srv.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
}
