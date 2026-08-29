package sitesync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

func TestSyncAccountAutoCompletesMaskedKeyBeforeModelDiscovery(t *testing.T) {
	ctx := setupProjectTestDB(t)
	keyDetailRequests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/token/" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"success":true,"data":{"items":[{"id":42,"name":"default-key","key":"sync*********-key","group":"default","status":1}]}}`))
		case r.URL.Path == "/api/token/42/key":
			keyDetailRequests++
			_, _ = w.Write([]byte(`{"success":true,"data":{"key":"sync-completed-key"}}`))
		case r.URL.Path == "/api/user/self/groups":
			_, _ = w.Write([]byte(`{"success":true,"data":[{"id":"default","name":"default"}]}`))
		case r.URL.Path == "/api/user_group_map":
			_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
		case r.URL.Path == "/models":
			if r.Header.Get("Authorization") != "Bearer sk-sync-completed-key" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"masked key was used for model discovery"}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	siteRecord := &model.Site{Name: "auto-complete-sync", Platform: model.SitePlatformNewAPI, BaseURL: server.URL, Enabled: true}
	if err := op.SiteCreate(siteRecord, ctx); err != nil {
		t.Fatalf("SiteCreate failed: %v", err)
	}
	account := &model.SiteAccount{
		SiteID:         siteRecord.ID,
		Name:           "sync-account",
		CredentialType: model.SiteCredentialTypeAccessToken,
		AccessToken:    "management-access-token",
		Enabled:        true,
		AutoSync:       true,
	}
	if err := op.SiteAccountCreate(account, ctx); err != nil {
		t.Fatalf("SiteAccountCreate failed: %v", err)
	}

	result, err := SyncAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("SyncAccount returned error: %v", err)
	}
	if result == nil || result.ModelCount != 1 || result.TokenCount != 1 {
		t.Fatalf("unexpected sync result: %+v", result)
	}
	if keyDetailRequests != 1 {
		t.Fatalf("expected sync to auto-complete the masked key once, got %d requests", keyDetailRequests)
	}

	reloaded, err := op.SiteAccountGet(account.ID, ctx)
	if err != nil {
		t.Fatalf("SiteAccountGet failed: %v", err)
	}
	if len(reloaded.Tokens) != 1 || reloaded.Tokens[0].Token != "sync-completed-key" || reloaded.Tokens[0].ValueStatus != model.SiteTokenValueStatusReady {
		t.Fatalf("expected an automatically completed ready key, got %+v", reloaded.Tokens)
	}

	secondResult, err := SyncAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("second SyncAccount returned error: %v", err)
	}
	if secondResult == nil || secondResult.ModelCount != 1 {
		t.Fatalf("unexpected second sync result: %+v", secondResult)
	}
	if keyDetailRequests != 1 {
		t.Fatalf("expected the second sync to reuse the stored ready key, got %d detail requests", keyDetailRequests)
	}
}

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
