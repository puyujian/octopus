package sitesync

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

const (
	sitePendingKeyCreateStatusCreated  = "created"
	sitePendingKeyCreateStatusExisting = "existing"
	sitePendingKeyCreateStatusFailed   = "failed"
	sitePendingKeySyncStatusNotNeeded  = "not_needed"
	sitePendingKeySyncStatusSuccess    = "success"
	sitePendingKeySyncStatusFailed     = "failed"
)

var siteAccountKeyCreateLocks sync.Map

func siteAccountKeyCreateLock(accountID int) *sync.Mutex {
	lock, _ := siteAccountKeyCreateLocks.LoadOrStore(accountID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func CreateAccountToken(ctx context.Context, accountID int, req model.SiteChannelKeyCreateRequest) (*model.SiteSyncResult, error) {
	lock := siteAccountKeyCreateLock(accountID)
	lock.Lock()
	defer lock.Unlock()

	siteRecord, account, err := loadSiteAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if siteRecord == nil || account == nil {
		return nil, fmt.Errorf("site account not found")
	}

	if err := createAccountTokenUpstream(ctx, siteRecord, account, req.GroupKey, req.Name); err != nil {
		return nil, err
	}

	return SyncAccount(ctx, accountID)
}

func CreatePendingAccountTokens(ctx context.Context, siteID int, accountID int) (*model.SiteChannelPendingKeyCreateResult, error) {
	lock := siteAccountKeyCreateLock(accountID)
	lock.Lock()
	defer lock.Unlock()

	siteRecord, account, err := loadSiteAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if siteRecord == nil || account == nil || siteRecord.ID != siteID || account.SiteID != siteID {
		return nil, fmt.Errorf("site account not found")
	}

	pendingGroups := pendingAccountTokenGroups(account)
	result := &model.SiteChannelPendingKeyCreateResult{
		SiteID:         siteID,
		AccountID:      accountID,
		AttemptedCount: len(pendingGroups),
		PendingCount:   len(pendingGroups),
		SyncStatus:     sitePendingKeySyncStatusNotNeeded,
		Results:        make([]model.SiteChannelPendingKeyCreateItem, 0, len(pendingGroups)),
	}
	if len(pendingGroups) == 0 {
		return result, nil
	}

	remoteTokens, err := fetchAccountTokensForCreatePreflight(ctx, siteRecord, account)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect existing site keys: %w", err)
	}
	remoteGroups := make(map[string]struct{}, len(remoteTokens))
	for _, token := range remoteTokens {
		remoteGroups[model.NormalizeSiteGroupKey(token.GroupKey)] = struct{}{}
	}

	for _, group := range pendingGroups {
		groupKey := model.NormalizeSiteGroupKey(group.GroupKey)
		item := model.SiteChannelPendingKeyCreateItem{
			GroupKey:  groupKey,
			GroupName: model.NormalizeSiteGroupName(groupKey, group.Name),
		}
		if _, exists := remoteGroups[groupKey]; exists {
			item.Status = sitePendingKeyCreateStatusExisting
			item.Message = "上游已存在该分组 Key，已跳过重复创建"
			result.ExistingCount++
			result.Results = append(result.Results, item)
			continue
		}

		if createErr := createAccountTokenUpstream(ctx, siteRecord, account, groupKey, ""); createErr != nil {
			item.Status = sitePendingKeyCreateStatusFailed
			item.Message = sanitizeSiteStatusText(createErr.Error())
			result.FailedCount++
			result.Results = append(result.Results, item)
			continue
		}

		remoteGroups[groupKey] = struct{}{}
		item.Status = sitePendingKeyCreateStatusCreated
		item.Message = "已创建"
		result.CreatedCount++
		result.Results = append(result.Results, item)
	}

	if result.CreatedCount+result.ExistingCount > 0 {
		result.SyncStatus = sitePendingKeySyncStatusSuccess
		if _, syncErr := SyncAccount(ctx, accountID); syncErr != nil {
			result.SyncStatus = sitePendingKeySyncStatusFailed
			result.SyncMessage = sanitizeSiteStatusText(syncErr.Error())
		} else {
			result.SyncMessage = "账号同步和渠道投影已完成"
		}
	}

	if accountView, viewErr := op.SiteChannelAccountGet(siteID, accountID, ctx); viewErr == nil {
		result.PendingCount = 0
		for _, group := range accountView.Groups {
			if !group.HasKeys {
				result.PendingCount++
			}
		}
	} else {
		result.PendingCount = result.FailedCount
		if result.SyncMessage == "" {
			result.SyncMessage = sanitizeSiteStatusText(viewErr.Error())
		}
	}

	return result, nil
}

func pendingAccountTokenGroups(account *model.SiteAccount) []model.SiteUserGroup {
	if account == nil {
		return nil
	}
	groupsWithTokens := make(map[string]struct{}, len(account.Tokens))
	for _, token := range account.Tokens {
		groupsWithTokens[model.NormalizeSiteGroupKey(token.GroupKey)] = struct{}{}
	}
	seen := make(map[string]struct{}, len(account.UserGroups))
	pending := make([]model.SiteUserGroup, 0)
	for _, group := range account.UserGroups {
		groupKey := model.NormalizeSiteGroupKey(group.GroupKey)
		if _, exists := groupsWithTokens[groupKey]; exists {
			continue
		}
		if _, duplicate := seen[groupKey]; duplicate {
			continue
		}
		seen[groupKey] = struct{}{}
		group.GroupKey = groupKey
		pending = append(pending, group)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].GroupKey < pending[j].GroupKey })
	return pending
}

func fetchAccountTokensForCreatePreflight(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount) ([]model.SiteToken, error) {
	if account == nil {
		return nil, fmt.Errorf("site account is nil")
	}
	if account.CredentialType == model.SiteCredentialTypeAPIKey {
		return nil, fmt.Errorf("API key credential account does not support quick site key creation")
	}

	switch siteRecord.Platform {
	case model.SitePlatformAnyRouter:
		accessToken, err := resolveAnyRouterManagedAccessToken(ctx, siteRecord, account)
		if err != nil {
			return nil, err
		}
		userID, _ := anyRouterDiscoverUserID(ctx, siteRecord, account, accessToken)
		return fetchAnyRouterManagementTokens(ctx, siteRecord, account, accessToken, userID)
	case model.SitePlatformNewAPI, model.SitePlatformOneAPI, model.SitePlatformOneHub, model.SitePlatformDoneHub:
		accessToken, err := resolveManagedAccessToken(ctx, siteRecord, account)
		if err != nil {
			return nil, err
		}
		return fetchManagementTokens(ctx, siteRecord, account, accessToken)
	case model.SitePlatformSub2API:
		accessToken, err := ensureFreshSub2APIAccessToken(ctx, siteRecord, account, false)
		if err != nil {
			return nil, err
		}
		return fetchSub2APITokens(ctx, siteRecord, account, accessToken)
	default:
		return nil, fmt.Errorf("site platform %s does not support quick key creation", siteRecord.Platform)
	}
}

func createAccountTokenUpstream(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount, rawGroupKey string, rawName string) error {
	groupKey := model.NormalizeSiteGroupKey(rawGroupKey)
	name := strings.TrimSpace(rawName)

	switch siteRecord.Platform {
	case model.SitePlatformAnyRouter:
		return createAnyRouterToken(ctx, siteRecord, account, groupKey, name)
	case model.SitePlatformNewAPI, model.SitePlatformOneAPI, model.SitePlatformOneHub, model.SitePlatformDoneHub:
		return createManagementPlatformToken(ctx, siteRecord, account, groupKey, name)
	case model.SitePlatformSub2API:
		return createSub2APIToken(ctx, siteRecord, account, groupKey, name)
	default:
		return fmt.Errorf("site platform %s does not support quick key creation", siteRecord.Platform)
	}
}

func createManagementPlatformToken(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount, groupKey string, name string) error {
	if account == nil {
		return fmt.Errorf("site account is nil")
	}
	if account.CredentialType == model.SiteCredentialTypeAPIKey {
		return fmt.Errorf("API key credential account does not support quick site key creation")
	}

	accessToken, err := resolveManagedAccessToken(ctx, siteRecord, account)
	if err != nil {
		return err
	}

	payload, err := requestJSONWithManagedAccessToken(
		ctx,
		siteRecord,
		http.MethodPost,
		buildSiteURL(siteRecord.BaseURL, "/api/token/"),
		buildManagedTokenCreatePayload(account, groupKey, name),
		accessToken,
		account,
	)
	if err != nil {
		return err
	}
	if !siteTokenCreateSucceeded(payload) {
		return fmt.Errorf("%s", firstNonEmptyString(extractSiteResponseMessage(payload), "site token creation failed"))
	}
	return nil
}

func createAnyRouterToken(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount, groupKey string, name string) error {
	if account == nil {
		return fmt.Errorf("site account is nil")
	}
	if account.CredentialType == model.SiteCredentialTypeAPIKey {
		return fmt.Errorf("API key credential account does not support quick site key creation")
	}

	accessToken, err := resolveAnyRouterManagedAccessToken(ctx, siteRecord, account)
	if err != nil {
		return err
	}

	payloadBody := buildManagedTokenCreatePayload(account, groupKey, name)
	requestURL := buildSiteURL(siteRecord.BaseURL, "/api/token/")

	userID, _ := anyRouterDiscoverUserID(ctx, siteRecord, account, accessToken)
	payload, _, err := anyRouterRequestJSONWithCookies(
		ctx,
		siteRecord,
		http.MethodPost,
		requestURL,
		payloadBody,
		anyRouterAuthHeaders(accessToken, userID),
		account,
	)
	if err == nil && siteTokenCreateSucceeded(payload) {
		return nil
	}

	tryUserIDs := []int{userID}
	if alternateUserID, probeErr := anyRouterProbeAlternateUserIDByCookie(ctx, siteRecord, account, accessToken, userID); probeErr == nil && alternateUserID > 0 {
		tryUserIDs = append(tryUserIDs, alternateUserID)
	}
	if userID <= 0 {
		if probedUserID, probeErr := anyRouterProbeUserIDByCookie(ctx, siteRecord, account, accessToken); probeErr == nil && probedUserID > 0 {
			tryUserIDs = append(tryUserIDs, probedUserID)
		}
	}
	tryUserIDs = slicesCompactInts(tryUserIDs)

	for _, candidateUserID := range tryUserIDs {
		for _, cookie := range anyRouterBuildCookieCandidates(accessToken) {
			headers := map[string]string{"Cookie": cookie}
			anyRouterAddUserIDHeaders(headers, candidateUserID)
			payload, _, requestErr := anyRouterRequestJSONWithCookies(
				ctx,
				siteRecord,
				http.MethodPost,
				requestURL,
				payloadBody,
				headers,
				account,
			)
			if requestErr != nil {
				if err == nil {
					err = requestErr
				}
				continue
			}
			if siteTokenCreateSucceeded(payload) {
				return nil
			}
			if message := strings.TrimSpace(extractSiteResponseMessage(payload)); message != "" {
				err = fmt.Errorf("%s", message)
			}
		}
	}

	if err != nil {
		return err
	}
	return fmt.Errorf("site token creation failed")
}

func createSub2APIToken(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount, groupKey string, name string) error {
	if account == nil {
		return fmt.Errorf("site account is nil")
	}
	if account.CredentialType == model.SiteCredentialTypeAPIKey {
		return fmt.Errorf("API key credential account does not support quick site key creation")
	}

	accessToken := strings.TrimSpace(account.AccessToken)
	accessToken, err := ensureFreshSub2APIAccessToken(ctx, siteRecord, account, false)
	if err != nil {
		return err
	}

	requestBody := buildSub2APITokenCreatePayload(account, groupKey, name)
	headers := map[string]string{"Authorization": ensureBearer(accessToken)}
	endpoints := []string{"/api/v1/keys", "/api/v1/api-keys"}
	var firstErr error

	for _, endpoint := range endpoints {
		payload, err := requestJSON(
			ctx,
			siteRecord,
			http.MethodPost,
			buildSiteURL(siteRecord.BaseURL, endpoint),
			requestBody,
			headers,
			account,
		)
		if err != nil {
			if shouldRetrySub2APIAfterRefresh(err, account) {
				refreshedToken, refreshErr := ensureFreshSub2APIAccessToken(ctx, siteRecord, account, true)
				if refreshErr == nil && stripBearerPrefix(refreshedToken) != stripBearerPrefix(accessToken) {
					headers = map[string]string{"Authorization": ensureBearer(refreshedToken)}
					payload, err = requestJSON(
						ctx,
						siteRecord,
						http.MethodPost,
						buildSiteURL(siteRecord.BaseURL, endpoint),
						requestBody,
						headers,
						account,
					)
					if err == nil {
						data, envelopeErr := unwrapSub2APIData(payload, endpoint)
						if envelopeErr == nil && siteTokenCreateSucceededFromAny(data) {
							return nil
						}
						if envelopeErr != nil && firstErr == nil {
							firstErr = envelopeErr
						}
					}
				}
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if data, envelopeErr := unwrapSub2APIData(payload, endpoint); envelopeErr == nil {
			if siteTokenCreateSucceededFromAny(data) {
				return nil
			}
		} else {
			return envelopeErr
		}
		if siteTokenCreateSucceeded(payload) {
			return nil
		}
		return fmt.Errorf("%s", firstNonEmptyString(extractSiteResponseMessage(payload), "site token creation failed"))
	}

	if firstErr != nil {
		return firstErr
	}
	return fmt.Errorf("site token creation failed")
}

func buildManagedTokenCreatePayload(account *model.SiteAccount, groupKey string, name string) map[string]any {
	return map[string]any{
		"name":                 defaultSiteTokenCreateName(account, groupKey, name),
		"unlimited_quota":      true,
		"expired_time":         -1,
		"remain_quota":         0,
		"allow_ips":            "",
		"model_limits_enabled": false,
		"model_limits":         "",
		"group":                model.NormalizeSiteGroupKey(groupKey),
	}
}

func buildSub2APITokenCreatePayload(account *model.SiteAccount, groupKey string, name string) map[string]any {
	payload := map[string]any{
		"name": defaultSiteTokenCreateName(account, groupKey, name),
	}
	groupKey = model.NormalizeSiteGroupKey(groupKey)
	if groupID, err := strconv.Atoi(groupKey); err == nil && groupID > 0 {
		payload["group_id"] = groupID
	}
	return payload
}

func defaultSiteTokenCreateName(account *model.SiteAccount, groupKey string, name string) string {
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		return trimmed
	}

	groupPart := strings.TrimSpace(groupKey)
	groupPart = strings.NewReplacer("/", "-", "\\", "-", " ", "-", "\t", "-", "\n", "-").Replace(groupPart)
	groupPart = strings.Trim(groupPart, "-")
	if groupPart == "" {
		groupPart = model.SiteDefaultGroupKey
	}
	return fmt.Sprintf("octopus-%s-%d", groupPart, time.Now().Unix())
}

func siteTokenCreateSucceeded(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	return siteTokenCreateSucceededFromAny(payload)
}

func siteTokenCreateSucceededFromAny(value any) bool {
	payload, ok := value.(map[string]any)
	if !ok {
		succeeded, ok := value.(bool)
		return ok && succeeded
	}
	if raw, ok := payload["success"]; ok {
		switch typed := raw.(type) {
		case bool:
			return typed
		case float64:
			return typed != 0
		case int:
			return typed != 0
		case string:
			switch strings.ToLower(strings.TrimSpace(typed)) {
			case "1", "true", "ok", "success":
				return true
			case "0", "false", "fail", "failed", "error":
				return false
			}
		}
		return false
	}
	return true
}

func slicesCompactInts(values []int) []int {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(values))
	result := make([]int, 0, len(values))
	for _, value := range values {
		if value < 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
