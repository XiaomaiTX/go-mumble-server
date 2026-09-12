package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExternalHTTPAuthorityAuthenticateAndResolve(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer service-secret" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/mumble/v1/authenticate":
			var request AuthenticateRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ServerInstanceID != "voice-01" || request.Password != "app-password" {
				t.Fatalf("unexpected request: %+v", request)
			}
			_, _ = w.Write([]byte(`{"code":200,"data":{"decision":"allow","user_id":173,"name":"Primary Pilot","groups":["authenticated","role:leader"],"identity_version":12,"policy_version":37}}`))
		case "/internal/mumble/v1/identities/resolve":
			_, _ = w.Write([]byte(`{"code":200,"data":{"identities":[{"eligible":true,"user_id":173,"name":"Primary Pilot","groups":["authenticated"],"identity_version":12,"policy_version":37}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	authority, err := NewExternalHTTPAuthority(ExternalHTTPConfig{BaseURL: server.URL, ServiceToken: "service-secret", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	got, err := authority.Authenticate(context.Background(), AuthenticateRequest{ServerInstanceID: "voice-01", Username: "Primary Pilot", Password: "app-password"})
	if err != nil || got.Decision != DecisionAllow || got.Identity.UserID != 173 || got.Identity.Name != "Primary Pilot" || len(got.Identity.Groups) != 2 {
		t.Fatalf("Authenticate = %+v, %v", got, err)
	}
	resolved, err := authority.Resolve(context.Background(), ResolveRequest{UserIDs: []uint32{173}})
	if err != nil || len(resolved) != 1 || !resolved[0].Eligible {
		t.Fatalf("Resolve = %+v, %v", resolved, err)
	}
}

func TestExternalHTTPAuthorityRejectsReservedIdentityAndTimesOut(t *testing.T) {
	t.Run("reserved id", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"code":200,"data":{"decision":"allow","user_id":0,"name":"SuperUser","groups":["role:admin"]}}`))
		}))
		defer server.Close()
		authority, _ := NewExternalHTTPAuthority(ExternalHTTPConfig{BaseURL: server.URL, ServiceToken: "token", Timeout: time.Second})
		if _, err := authority.Authenticate(context.Background(), AuthenticateRequest{}); err == nil {
			t.Fatal("reserved external identity must be rejected")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{"code":200,"data":{"decision":"deny"}}`))
		}))
		defer server.Close()
		authority, _ := NewExternalHTTPAuthority(ExternalHTTPConfig{BaseURL: server.URL, ServiceToken: "token", Timeout: 10 * time.Millisecond})
		if _, err := authority.Authenticate(context.Background(), AuthenticateRequest{}); err == nil {
			t.Fatal("timeout must fail closed")
		}
	})
}
