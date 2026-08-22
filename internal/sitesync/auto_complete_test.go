package sitesync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

func TestFetchRemoteTokenKeyForAutoCompletionUsesPostKeyEndpoint(t *testing.T) {
	platformUserID := 1404
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid URL (GET /api/token/16424/key)"}}`))
			return
		}
		if r.URL.Path != "/api/token/16424/key" {
			t.Fatalf("expected key endpoint, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Fatalf("expected managed access token header, got %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("New-API-User") != "1404" {
			t.Fatalf("expected New-API-User header, got %q", r.Header.Get("New-API-User"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"key":"LnnVfullJJEg"},"success":true}`))
	}))
	defer server.Close()

	fullToken, err := fetchRemoteTokenKeyForAutoCompletion(
		context.Background(),
		&model.Site{Platform: model.SitePlatformNewAPI, BaseURL: server.URL},
		&model.SiteAccount{
			CredentialType: model.SiteCredentialTypeAccessToken,
			AccessToken:    "test-access-token",
			PlatformUserID: &platformUserID,
		},
		siteRemoteToken{ID: 16424, Token: "LnnV**********JJEg"},
	)
	if err != nil {
		t.Fatalf("fetchRemoteTokenKeyForAutoCompletion returned error: %v", err)
	}
	if fullToken != "LnnVfullJJEg" {
		t.Fatalf("expected full token, got %q", fullToken)
	}
}

func TestFetchRemoteTokenKeyForAutoCompletionFallsBackToGet(t *testing.T) {
	platformUserID := 1404
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid URL (POST /api/token/16424/key)"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"key":"LnnVfullJJEg"},"success":true}`))
	}))
	defer server.Close()

	fullToken, err := fetchRemoteTokenKeyForAutoCompletion(
		context.Background(),
		&model.Site{Platform: model.SitePlatformNewAPI, BaseURL: server.URL},
		&model.SiteAccount{
			CredentialType: model.SiteCredentialTypeAccessToken,
			AccessToken:    "test-access-token",
			PlatformUserID: &platformUserID,
		},
		siteRemoteToken{ID: 16424, Token: "LnnV**********JJEg"},
	)
	if err != nil {
		t.Fatalf("fetchRemoteTokenKeyForAutoCompletion returned error: %v", err)
	}
	if fullToken != "LnnVfullJJEg" {
		t.Fatalf("expected full token after GET fallback, got %q", fullToken)
	}
}
