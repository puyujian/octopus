package sitesync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dbpkg "github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

type pendingKeyUpstream struct {
	mu             sync.Mutex
	created        map[string]string
	failGroups     map[string]string
	createRequests int
}

func newPendingKeyUpstream(initialGroups ...string) *pendingKeyUpstream {
	created := make(map[string]string, len(initialGroups))
	for _, groupKey := range initialGroups {
		created[groupKey] = "sk-existing-" + groupKey
	}
	return &pendingKeyUpstream{created: created, failGroups: make(map[string]string)}
}

func (u *pendingKeyUpstream) handler(t *testing.T, groups []string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/user/self":
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":1,"username":"batch-user"}}`))
		case r.URL.Path == "/api/token/" && r.Method == http.MethodGet:
			u.mu.Lock()
			items := make([]map[string]any, 0, len(u.created))
			id := 1
			for _, groupKey := range groups {
				key, ok := u.created[groupKey]
				if !ok {
					continue
				}
				items = append(items, map[string]any{"id": id, "name": "key-" + groupKey, "key": key, "group": groupKey, "status": 1})
				id++
			}
			u.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": items}})
		case r.URL.Path == "/api/token/" && r.Method == http.MethodPost:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create body failed: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			groupKey := model.NormalizeSiteGroupKey(jsonString(body["group"]))
			u.mu.Lock()
			u.createRequests++
			failure := u.failGroups[groupKey]
			if failure == "" {
				u.created[groupKey] = "sk-created-" + groupKey
			}
			u.mu.Unlock()
			if failure != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "message": failure})
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":99}}`))
		case r.URL.Path == "/api/user/self/groups":
			items := make([]map[string]any, 0, len(groups))
			for _, groupKey := range groups {
				items = append(items, map[string]any{"id": groupKey, "name": strings.ToUpper(groupKey)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
		case r.URL.Path == "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-batch"}]}`))
		default:
			http.NotFound(w, r)
		}
	}
}

func createPendingKeyAccount(t *testing.T, baseURL string, groups ...string) (context.Context, *model.Site, *model.SiteAccount) {
	t.Helper()
	ctx := setupProjectTestDB(t)
	site := &model.Site{Name: "batch-create-site", Platform: model.SitePlatformNewAPI, BaseURL: baseURL, Enabled: true}
	if err := op.SiteCreate(site, ctx); err != nil {
		t.Fatalf("SiteCreate failed: %v", err)
	}
	account := &model.SiteAccount{
		SiteID: site.ID, Name: "batch-create-account", CredentialType: model.SiteCredentialTypeAccessToken,
		AccessToken: "batch-access-token", Enabled: true, AutoSync: true,
	}
	if err := op.SiteAccountCreate(account, ctx); err != nil {
		t.Fatalf("SiteAccountCreate failed: %v", err)
	}
	for _, groupKey := range groups {
		group := &model.SiteUserGroup{SiteAccountID: account.ID, GroupKey: groupKey, Name: strings.ToUpper(groupKey)}
		if err := dbpkg.GetDB().WithContext(ctx).Create(group).Error; err != nil {
			t.Fatalf("create site user group failed: %v", err)
		}
	}
	return ctx, site, account
}

func TestCreatePendingAccountTokensCreatesAllAndSyncsOnce(t *testing.T) {
	upstream := newPendingKeyUpstream()
	groups := []string{"alpha", "beta"}
	server := httptest.NewServer(upstream.handler(t, groups))
	defer server.Close()
	ctx, site, account := createPendingKeyAccount(t, server.URL, groups...)

	result, err := CreatePendingAccountTokens(ctx, site.ID, account.ID)
	if err != nil {
		t.Fatalf("CreatePendingAccountTokens returned error: %v", err)
	}
	if result.AttemptedCount != 2 || result.CreatedCount != 2 || result.ExistingCount != 0 || result.FailedCount != 0 || result.PendingCount != 0 {
		t.Fatalf("unexpected batch result: %+v", result)
	}
	if result.SyncStatus != sitePendingKeySyncStatusSuccess {
		t.Fatalf("expected successful sync, got %+v", result)
	}
	if upstream.createRequests != 2 {
		t.Fatalf("expected two upstream creates, got %d", upstream.createRequests)
	}
	reloaded, err := op.SiteAccountGet(account.ID, ctx)
	if err != nil {
		t.Fatalf("SiteAccountGet failed: %v", err)
	}
	if len(reloaded.Tokens) != 2 {
		t.Fatalf("expected two synchronized tokens, got %+v", reloaded.Tokens)
	}
}

func TestCreatePendingAccountTokensContinuesAfterFailure(t *testing.T) {
	upstream := newPendingKeyUpstream()
	upstream.failGroups["alpha"] = "alpha denied"
	groups := []string{"alpha", "beta"}
	server := httptest.NewServer(upstream.handler(t, groups))
	defer server.Close()
	ctx, site, account := createPendingKeyAccount(t, server.URL, groups...)

	result, err := CreatePendingAccountTokens(ctx, site.ID, account.ID)
	if err != nil {
		t.Fatalf("CreatePendingAccountTokens returned error: %v", err)
	}
	if result.CreatedCount != 1 || result.FailedCount != 1 || result.PendingCount != 1 {
		t.Fatalf("unexpected partial batch result: %+v", result)
	}
	if result.Results[0].GroupKey != "alpha" || result.Results[0].Status != sitePendingKeyCreateStatusFailed || result.Results[1].GroupKey != "beta" || result.Results[1].Status != sitePendingKeyCreateStatusCreated {
		t.Fatalf("unexpected per-group results: %+v", result.Results)
	}
	if upstream.createRequests != 2 {
		t.Fatalf("expected processing to continue after failure, got %d create requests", upstream.createRequests)
	}
}

func TestCreatePendingAccountTokensSkipsExistingUpstreamKey(t *testing.T) {
	upstream := newPendingKeyUpstream("alpha")
	groups := []string{"alpha"}
	server := httptest.NewServer(upstream.handler(t, groups))
	defer server.Close()
	ctx, site, account := createPendingKeyAccount(t, server.URL, groups...)

	result, err := CreatePendingAccountTokens(ctx, site.ID, account.ID)
	if err != nil {
		t.Fatalf("CreatePendingAccountTokens returned error: %v", err)
	}
	if result.CreatedCount != 0 || result.ExistingCount != 1 || result.FailedCount != 0 || result.PendingCount != 0 {
		t.Fatalf("unexpected existing-key result: %+v", result)
	}
	if upstream.createRequests != 0 {
		t.Fatalf("expected no duplicate upstream create, got %d", upstream.createRequests)
	}
}

func TestCreatePendingAccountTokensSerializesConcurrentRequests(t *testing.T) {
	upstream := newPendingKeyUpstream()
	groups := []string{"alpha"}
	server := httptest.NewServer(upstream.handler(t, groups))
	defer server.Close()
	ctx, site, account := createPendingKeyAccount(t, server.URL, groups...)

	results := make(chan *model.SiteChannelPendingKeyCreateResult, 2)
	errors := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := CreatePendingAccountTokens(ctx, site.ID, account.ID)
			results <- result
			errors <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent CreatePendingAccountTokens returned error: %v", err)
		}
	}
	attempted := 0
	created := 0
	for result := range results {
		attempted += result.AttemptedCount
		created += result.CreatedCount
	}
	if attempted != 1 || created != 1 || upstream.createRequests != 1 {
		t.Fatalf("expected one serialized creation, attempted=%d created=%d upstream=%d", attempted, created, upstream.createRequests)
	}
}

func TestCreateAccountTokenCreatesManagedKeyAndSyncsAccount(t *testing.T) {
	ctx := setupProjectTestDB(t)

	var createdBody map[string]any
	keyDetailRequests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/api/user/self":
			if r.Header.Get("Authorization") != "Bearer test-access-token" || r.Header.Get("New-API-User") != "11494" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"message":"无权进行此操作，未提供 New-Api-User"}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":11494,"username":"managed-user"}}`))
		case r.URL.Path == "/api/token/" && r.Method == http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer test-access-token" || r.Header.Get("New-API-User") != "11494" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"message":"无权进行此操作，未提供 New-Api-User"}`))
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&createdBody); err != nil {
				t.Fatalf("decode create token body failed: %v", err)
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":1}}`))
		case r.URL.Path == "/api/token/" && r.Method == http.MethodGet:
			if r.Header.Get("Authorization") != "Bearer test-access-token" || r.Header.Get("New-API-User") != "11494" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"message":"无权进行此操作，未提供 New-Api-User"}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"items":[{"id":1,"name":"vip-created","key":"mana***********-key","group":"vip","status":1}]}}`))
		case r.URL.Path == "/api/token/1/key":
			if r.Header.Get("Authorization") != "Bearer test-access-token" || r.Header.Get("New-API-User") != "11494" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"message":"无权进行此操作，未提供 New-Api-User"}`))
				return
			}
			keyDetailRequests++
			_, _ = w.Write([]byte(`{"success":true,"data":{"key":"managed-created-key"}}`))
		case r.URL.Path == "/api/user/self/groups":
			if r.Header.Get("Authorization") != "Bearer test-access-token" || r.Header.Get("New-API-User") != "11494" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"message":"无权进行此操作，未提供 New-Api-User"}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"vip","name":"VIP"}]}`))
		case r.URL.Path == "/models":
			if r.Header.Get("Authorization") != "Bearer sk-managed-created-key" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	site := &model.Site{
		Name:     "managed-create-site",
		Platform: model.SitePlatformNewAPI,
		BaseURL:  server.URL,
		Enabled:  true,
	}
	if err := op.SiteCreate(site, ctx); err != nil {
		t.Fatalf("SiteCreate failed: %v", err)
	}

	account := &model.SiteAccount{
		SiteID:         site.ID,
		Name:           "managed-create-account",
		CredentialType: model.SiteCredentialTypeAccessToken,
		AccessToken:    "test-access-token",
		Enabled:        true,
		AutoSync:       true,
	}
	if err := op.SiteAccountCreate(account, ctx); err != nil {
		t.Fatalf("SiteAccountCreate failed: %v", err)
	}

	result, err := CreateAccountToken(ctx, account.ID, model.SiteChannelKeyCreateRequest{GroupKey: "vip"})
	if err != nil {
		t.Fatalf("CreateAccountToken returned error: %v", err)
	}
	if result == nil || result.TokenCount != 1 {
		t.Fatalf("unexpected sync result: %+v", result)
	}
	if keyDetailRequests != 1 {
		t.Fatalf("expected quick creation sync to auto-complete the masked key once, got %d requests", keyDetailRequests)
	}
	if createdBody["group"] != "vip" {
		t.Fatalf("expected created group to be vip, got %#v", createdBody["group"])
	}
	if createdBody["unlimited_quota"] != true {
		t.Fatalf("expected unlimited_quota=true, got %#v", createdBody["unlimited_quota"])
	}
	createdName, _ := createdBody["name"].(string)
	if !strings.HasPrefix(createdName, "octopus-vip-") {
		t.Fatalf("expected generated token name to start with octopus-vip-, got %q", createdName)
	}

	reloaded, err := op.SiteAccountGet(account.ID, ctx)
	if err != nil {
		t.Fatalf("SiteAccountGet failed: %v", err)
	}
	if len(reloaded.Tokens) != 1 || reloaded.Tokens[0].GroupKey != "vip" || reloaded.Tokens[0].Token != "managed-created-key" || reloaded.Tokens[0].ValueStatus != model.SiteTokenValueStatusReady {
		t.Fatalf("unexpected synced tokens: %+v", reloaded.Tokens)
	}
	if len(reloaded.UserGroups) != 1 || reloaded.UserGroups[0].GroupKey != "vip" {
		t.Fatalf("unexpected synced groups: %+v", reloaded.UserGroups)
	}
	if len(reloaded.Models) != 1 || reloaded.Models[0].GroupKey != "vip" || reloaded.Models[0].ModelName != "gpt-4o-mini" {
		t.Fatalf("unexpected synced models: %+v", reloaded.Models)
	}
}

func TestCreateAccountTokenCreatesSub2APIKeyAndSyncsAccount(t *testing.T) {
	ctx := setupProjectTestDB(t)

	var createdBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/api/v1/keys" && r.Method == http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer sub2api-token" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"message":"unauthorized"}`))
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&createdBody); err != nil {
				t.Fatalf("decode sub2api create body failed: %v", err)
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":31}}`))
		case r.URL.Path == "/api/v1/keys":
			if r.Header.Get("Authorization") != "Bearer sub2api-token" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"message":"unauthorized"}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"name":"sub2api-created","key":"sub2api-created-key","group_id":"7","group_name":"VIP 7","status":1}]}`))
		case r.URL.Path == "/models":
			if r.Header.Get("Authorization") != "Bearer sub2api-created-key" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	site := &model.Site{
		Name:     "sub2api-create-site",
		Platform: model.SitePlatformSub2API,
		BaseURL:  server.URL,
		Enabled:  true,
	}
	if err := op.SiteCreate(site, ctx); err != nil {
		t.Fatalf("SiteCreate failed: %v", err)
	}

	account := &model.SiteAccount{
		SiteID:         site.ID,
		Name:           "sub2api-create-account",
		CredentialType: model.SiteCredentialTypeAccessToken,
		AccessToken:    "sub2api-token",
		Enabled:        true,
		AutoSync:       true,
	}
	if err := op.SiteAccountCreate(account, ctx); err != nil {
		t.Fatalf("SiteAccountCreate failed: %v", err)
	}

	result, err := CreateAccountToken(context.Background(), account.ID, model.SiteChannelKeyCreateRequest{
		GroupKey: "7",
		Name:     "manual-sub2api-name",
	})
	if err != nil {
		t.Fatalf("CreateAccountToken returned error: %v", err)
	}
	if result == nil || result.TokenCount != 1 {
		t.Fatalf("unexpected sync result: %+v", result)
	}
	if createdBody["group_id"] != float64(7) && createdBody["group_id"] != 7 {
		t.Fatalf("expected group_id=7, got %#v", createdBody["group_id"])
	}
	if createdBody["name"] != "manual-sub2api-name" {
		t.Fatalf("expected provided token name to be used, got %#v", createdBody["name"])
	}

	reloaded, err := op.SiteAccountGet(account.ID, ctx)
	if err != nil {
		t.Fatalf("SiteAccountGet failed: %v", err)
	}
	if len(reloaded.Tokens) != 1 || reloaded.Tokens[0].GroupKey != "7" || reloaded.Tokens[0].Token != "sub2api-created-key" {
		t.Fatalf("unexpected synced tokens: %+v", reloaded.Tokens)
	}
	if len(reloaded.UserGroups) != 1 || reloaded.UserGroups[0].GroupKey != "7" {
		t.Fatalf("unexpected synced groups: %+v", reloaded.UserGroups)
	}
}

func TestSiteTokenCreateSucceededFromAnyRequiresExplicitPrimitiveTrue(t *testing.T) {
	for name, value := range map[string]any{
		"false":        false,
		"zero":         0,
		"empty string": "",
		"string true":  "true",
		"non-empty":    "ok",
	} {
		if siteTokenCreateSucceededFromAny(value) {
			t.Fatalf("expected %s primitive to be unsuccessful", name)
		}
	}
	if !siteTokenCreateSucceededFromAny(true) {
		t.Fatalf("expected boolean true primitive to be successful")
	}
}
