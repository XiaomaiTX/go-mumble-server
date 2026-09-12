package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/database"
)

func TestIdentityRevalidationEndpointRequiresDedicatedTokenAndPullsTruth(t *testing.T) {
	var gotUserID uint32
	db, err := database.Open(filepath.Join(t.TempDir(), "seat-revalidate.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	router := RouterWithMumbleAndIdentity(db, &config.Config{IdentityRevalidateToken: "provider-push-secret"}, nil, nil, nil, nil, nil, nil, nil, nil, nil, func(_ context.Context, userID uint32) error {
		gotUserID = userID
		return nil
	})

	unauthorized := httptest.NewRequest(http.MethodPost, "/internal/identity/v1/revalidate", strings.NewReader(`{"user_id":173}`))
	unauthorized.Header.Set("Authorization", "Bearer seat-user-jwt")
	unauthorizedRecorder := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedRecorder, unauthorized)
	if unauthorizedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorizedRecorder.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/internal/identity/v1/revalidate", strings.NewReader(`{"user_id":173}`))
	request.Header.Set("Authorization", "Bearer provider-push-secret")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || gotUserID != 173 {
		t.Fatalf("status=%d user_id=%d body=%s", recorder.Code, gotUserID, recorder.Body.String())
	}
}
