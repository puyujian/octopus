package sitesync

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

type siteRemoteToken struct {
	ID        int
	Name      string
	Token     string
	GroupKey  string
	GroupName string
}

// AutoCompleteSiteSourceKeys resolves masked upstream keys with the site's own
// authenticated management API. The auto-completion response contains only
// counts and status; the full key is consumed by the server-side update path.
func AutoCompleteSiteSourceKeys(ctx context.Context, siteID int, accountID int) (*model.SiteSourceKeyAutoCompletionResult, error) {
	siteRecord, account, err := loadSiteAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if siteRecord.ID != siteID {
		return nil, fmt.Errorf("site account does not belong to site")
	}

	pending := make([]model.SiteToken, 0)
	for _, token := range account.Tokens {
		if model.NormalizeSiteTokenValueStatus(token.ValueStatus, token.Token) == model.SiteTokenValueStatusMaskedPending {
			pending = append(pending, token)
		}
	}

	result := &model.SiteSourceKeyAutoCompletionResult{
		SiteID:         siteID,
		AccountID:      accountID,
		AttemptedCount: len(pending),
		PendingCount:   len(pending),
	}
	if len(pending) == 0 {
		result.Message = "没有待自动补全的 Key"
		return result, nil
	}

	remoteTokens, err := fetchRemoteTokensForAutoCompletion(ctx, siteRecord, account)
	if err != nil {
		return nil, err
	}

	for _, localToken := range pending {
		remoteToken, ok := matchRemoteTokenForAutoCompletion(localToken, remoteTokens)
		if !ok || remoteToken.ID <= 0 {
			continue
		}

		fullToken, resolveErr := fetchRemoteTokenKeyForAutoCompletion(ctx, siteRecord, account, remoteToken)
		if resolveErr != nil || model.IsMaskedSiteTokenValue(fullToken) || !model.SiteMaskedTokenMatches(fullToken, localToken.Token) {
			continue
		}

		enabled := true
		if updateErr := op.UpdateSiteSourceKeys(siteID, accountID, &model.SiteSourceKeyUpdateRequest{
			GroupKey: localToken.GroupKey,
			KeysToUpdate: []model.SiteSourceKeyUpdateItem{{
				ID:      localToken.ID,
				Enabled: &enabled,
				Token:   &fullToken,
			}},
		}, ctx); updateErr != nil {
			continue
		}
		result.CompletedCount++
	}

	result.PendingCount -= result.CompletedCount
	if result.CompletedCount > 0 {
		if _, err := ProjectAccount(ctx, accountID); err != nil {
			return nil, fmt.Errorf("自动补全 Key 已保存，但恢复投影失败: %w", err)
		}
	}

	switch {
	case result.PendingCount == 0:
		result.Message = fmt.Sprintf("自动补全完成：已恢复 %d 个 Key", result.CompletedCount)
	case result.CompletedCount > 0:
		result.Message = fmt.Sprintf("自动补全完成：恢复 %d 个 Key，仍有 %d 个 Key 待处理", result.CompletedCount, result.PendingCount)
	default:
		result.Message = "未能从站点管理接口匹配到完整 Key，请确认当前账号有权限读取 Key"
	}
	return result, nil
}

func fetchRemoteTokensForAutoCompletion(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount) ([]siteRemoteToken, error) {
	switch siteRecord.Platform {
	case model.SitePlatformNewAPI, model.SitePlatformOneAPI, model.SitePlatformOneHub, model.SitePlatformDoneHub:
		accessToken, err := resolveManagedAccessToken(ctx, siteRecord, account)
		if err != nil {
			return nil, err
		}
		payload, err := requestJSONWithManagedAccessToken(ctx, siteRecord, http.MethodGet, buildSiteURL(siteRecord.BaseURL, "/api/token/?p=0&size=100"), nil, accessToken, account)
		if err != nil {
			return nil, err
		}
		return parseRemoteTokensForAutoCompletion(payload), nil

	case model.SitePlatformAnyRouter:
		accessToken, err := resolveAnyRouterManagedAccessToken(ctx, siteRecord, account)
		if err != nil {
			return nil, err
		}
		userID, _ := anyRouterDiscoverUserID(ctx, siteRecord, account, accessToken)
		payload, _, err := anyRouterRequestJSONWithCookies(ctx, siteRecord, http.MethodGet, buildSiteURL(siteRecord.BaseURL, "/api/token/?p=0&size=100"), nil, anyRouterAuthHeaders(accessToken, userID), account)
		if err != nil {
			return nil, err
		}
		return parseRemoteTokensForAutoCompletion(payload), nil

	case model.SitePlatformSub2API:
		accessToken, err := ensureFreshSub2APIAccessToken(ctx, siteRecord, account, false)
		if err != nil {
			return nil, err
		}
		return fetchSub2APIRemoteTokensForAutoCompletion(ctx, siteRecord, account, accessToken)

	default:
		return nil, fmt.Errorf("平台 %s 不支持自动读取站点 Key", siteRecord.Platform)
	}
}

func parseRemoteTokensForAutoCompletion(payload map[string]any) []siteRemoteToken {
	items := parseTokenItems(payload)
	result := make([]siteRemoteToken, 0, len(items))
	for _, item := range items {
		id := int(anyToInt64(firstNonNilValue(item, "id", "token_id", "tokenId", "key_id", "keyId")))
		if id <= 0 {
			continue
		}
		value := firstNonEmptyString(
			jsonString(item["key"]),
			jsonString(item["token"]),
			jsonString(item["api_key"]),
			jsonString(item["apiKey"]),
		)
		if value == "" {
			continue
		}
		groupKey := model.NormalizeSiteGroupKey(firstNonEmptyString(
			jsonString(item["group"]),
			jsonString(item["token_group"]),
			jsonString(item["tokenGroup"]),
			jsonString(item["group_id"]),
			jsonString(item["groupId"]),
			jsonString(item["group_name"]),
			jsonString(item["groupName"]),
		))
		groupName := model.NormalizeSiteGroupName(groupKey, firstNonEmptyString(
			jsonString(item["group_name"]),
			jsonString(item["groupName"]),
			jsonString(item["group"]),
			jsonString(item["token_group"]),
		))
		result = append(result, siteRemoteToken{
			ID:        id,
			Name:      strings.TrimSpace(jsonString(item["name"])),
			Token:     strings.TrimSpace(value),
			GroupKey:  groupKey,
			GroupName: groupName,
		})
	}
	return result
}

func firstNonNilValue(item map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := item[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func matchRemoteTokenForAutoCompletion(local model.SiteToken, remoteTokens []siteRemoteToken) (siteRemoteToken, bool) {
	groupKey := model.NormalizeSiteGroupKey(local.GroupKey)
	candidates := make([]siteRemoteToken, 0)
	for _, remote := range remoteTokens {
		if model.NormalizeSiteGroupKey(remote.GroupKey) == groupKey {
			candidates = append(candidates, remote)
		}
	}
	if len(candidates) == 0 {
		return siteRemoteToken{}, false
	}

	name := strings.TrimSpace(local.Name)
	if name != "" {
		named := make([]siteRemoteToken, 0, len(candidates))
		for _, candidate := range candidates {
			if strings.EqualFold(strings.TrimSpace(candidate.Name), name) {
				named = append(named, candidate)
			}
		}
		if len(named) > 0 {
			candidates = named
		}
	}

	matches := make([]siteRemoteToken, 0, len(candidates))
	for _, candidate := range candidates {
		if autoCompletionTokenValuesMatch(candidate.Token, local.Token) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	if len(matches) == 0 && len(candidates) == 1 {
		return candidates[0], true
	}
	return siteRemoteToken{}, false
}

func autoCompletionTokenValuesMatch(remoteValue string, localValue string) bool {
	remoteValue = strings.TrimSpace(remoteValue)
	localValue = strings.TrimSpace(localValue)
	if remoteValue == localValue {
		return true
	}
	if model.IsMaskedSiteTokenValue(remoteValue) && model.IsMaskedSiteTokenValue(localValue) {
		return model.NormalizeComparableSiteTokenValue(remoteValue) == model.NormalizeComparableSiteTokenValue(localValue)
	}
	return model.SiteMaskedTokenMatches(remoteValue, localValue)
}

func fetchRemoteTokenKeyForAutoCompletion(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount, remoteToken siteRemoteToken) (string, error) {
	if !model.IsMaskedSiteTokenValue(remoteToken.Token) {
		return strings.TrimSpace(remoteToken.Token), nil
	}

	switch siteRecord.Platform {
	case model.SitePlatformNewAPI, model.SitePlatformOneAPI, model.SitePlatformOneHub, model.SitePlatformDoneHub:
		accessToken, err := resolveManagedAccessToken(ctx, siteRecord, account)
		if err != nil {
			return "", err
		}
		// 兼容不同 new-api 系实现：部分站点仅支持 POST（如 runanytime.hxi.me），
		// 部分站点仅支持 GET（如 wzw.pp.ua），依次尝试直到取到完整 Key。
		return fetchManagedRemoteTokenKey(ctx, siteRecord, account, accessToken, remoteToken)

	case model.SitePlatformAnyRouter:
		accessToken, err := resolveAnyRouterManagedAccessToken(ctx, siteRecord, account)
		if err != nil {
			return "", err
		}
		userID, _ := anyRouterDiscoverUserID(ctx, siteRecord, account, accessToken)
		return fetchAnyRouterRemoteTokenKey(ctx, siteRecord, account, accessToken, userID, remoteToken)

	case model.SitePlatformSub2API:
		accessToken, err := ensureFreshSub2APIAccessToken(ctx, siteRecord, account, false)
		if err != nil {
			return "", err
		}
		return fetchSub2APIRemoteTokenKey(ctx, siteRecord, account, accessToken, remoteToken)

	default:
		return "", fmt.Errorf("平台 %s 不支持自动读取站点 Key", siteRecord.Platform)
	}
}

func fetchManagedRemoteTokenKey(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount, accessToken string, remoteToken siteRemoteToken) (string, error) {
	requestURL := buildSiteURL(siteRecord.BaseURL, fmt.Sprintf("/api/token/%d/key", remoteToken.ID))
	var firstErr error
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		payload, err := requestJSONWithManagedAccessToken(ctx, siteRecord, method, requestURL, nil, accessToken, account)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if key := extractRemoteFullToken(payload); key != "" && !model.IsMaskedSiteTokenValue(key) {
			return key, nil
		}
	}
	if firstErr != nil {
		return "", firstErr
	}
	return "", fmt.Errorf("上游未返回完整 Key")
}

func fetchAnyRouterRemoteTokenKey(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount, accessToken string, userID int, remoteToken siteRemoteToken) (string, error) {
	requestURL := buildSiteURL(siteRecord.BaseURL, fmt.Sprintf("/api/token/%d/key", remoteToken.ID))
	var firstErr error
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		payload, _, err := anyRouterRequestJSONWithCookies(ctx, siteRecord, method, requestURL, nil, anyRouterAuthHeaders(accessToken, userID), account)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if key := extractRemoteFullToken(payload); key != "" && !model.IsMaskedSiteTokenValue(key) {
			return key, nil
		}
	}
	if firstErr != nil {
		return "", firstErr
	}
	return "", fmt.Errorf("上游未返回完整 Key")
}

func extractRemoteFullToken(payload map[string]any) string {
	return firstNonEmptyString(
		jsonString(payload["key"]),
		jsonString(nestedValue(payload, "data", "key")),
		jsonString(payload["token"]),
		jsonString(nestedValue(payload, "data", "token")),
	)
}

func fetchSub2APIRemoteTokensForAutoCompletion(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount, accessToken string) ([]siteRemoteToken, error) {
	endpoints := []string{"/api/v1/keys?page=1&page_size=100", "/api/v1/api-keys?page=1&page_size=100", "/api/v1/keys", "/api/v1/api-keys"}
	var firstErr error
	for _, endpoint := range endpoints {
		payload, err := requestJSON(ctx, siteRecord, http.MethodGet, buildSiteURL(siteRecord.BaseURL, endpoint), nil, map[string]string{"Authorization": ensureBearer(accessToken)}, account)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		data, err := unwrapSub2APIData(payload, endpoint)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		items := parseTokenItemsFromAny(data)
		result := parseRemoteTokensForAutoCompletion(map[string]any{"data": items})
		if len(result) > 0 {
			return result, nil
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, nil
}

func fetchSub2APIRemoteTokenKey(ctx context.Context, siteRecord *model.Site, account *model.SiteAccount, accessToken string, remoteToken siteRemoteToken) (string, error) {
	endpoints := []string{
		fmt.Sprintf("/api/v1/keys/%d", remoteToken.ID),
		fmt.Sprintf("/api/v1/api-keys/%d", remoteToken.ID),
		fmt.Sprintf("/api/v1/keys/%d/key", remoteToken.ID),
		fmt.Sprintf("/api/v1/api-keys/%d/key", remoteToken.ID),
	}
	var firstErr error
	for _, endpoint := range endpoints {
		payload, err := requestJSON(ctx, siteRecord, http.MethodGet, buildSiteURL(siteRecord.BaseURL, endpoint), nil, map[string]string{"Authorization": ensureBearer(accessToken)}, account)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if key := extractRemoteFullToken(payload); key != "" {
			return key, nil
		}
	}
	if firstErr != nil {
		return "", firstErr
	}
	return "", fmt.Errorf("sub2api did not return a full Key")
}
