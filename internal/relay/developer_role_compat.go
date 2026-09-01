package relay

import (
	"net/http"
	"strings"
	"sync"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

type developerRoleCompatibilityKey struct {
	channelID int
	model     string
}

var developerRoleSystemCompatibility sync.Map

func developerRoleCompatibilityKeyFor(channelID int, modelName string) developerRoleCompatibilityKey {
	return developerRoleCompatibilityKey{
		channelID: channelID,
		model:     strings.ToLower(strings.TrimSpace(modelName)),
	}
}

func channelModelRequiresSystemRole(channelID int, modelName string) bool {
	_, ok := developerRoleSystemCompatibility.Load(developerRoleCompatibilityKeyFor(channelID, modelName))
	return ok
}

func rememberChannelModelRequiresSystemRole(channelID int, modelName string) {
	developerRoleSystemCompatibility.Store(developerRoleCompatibilityKeyFor(channelID, modelName), struct{}{})
}

func resetDeveloperRoleCompatibilityCache() {
	developerRoleSystemCompatibility.Range(func(key, _ any) bool {
		developerRoleSystemCompatibility.Delete(key)
		return true
	})
}

func requestWithDeveloperRoleDowngraded(request *model.InternalLLMRequest) (*model.InternalLLMRequest, bool) {
	if request == nil || len(request.Messages) == 0 {
		return request, false
	}

	clone := *request
	clone.Messages = append([]model.Message(nil), request.Messages...)
	changed := false
	for index := range clone.Messages {
		if clone.Messages[index].Role != "developer" {
			continue
		}
		clone.Messages[index].Role = "system"
		changed = true
	}
	if !changed {
		return request, false
	}
	return &clone, true
}

func upstreamRejectsDeveloperRole(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest || len(body) == 0 {
		return false
	}
	normalized := strings.ToLower(string(body))
	if !strings.Contains(normalized, "developer") {
		return false
	}
	normalized = strings.ReplaceAll(normalized, `\"`, `"`)
	return strings.Contains(normalized, "developer is not one of") ||
		strings.Contains(normalized, "unsupported value: 'developer'") ||
		strings.Contains(normalized, `unsupported value: "developer"`) ||
		strings.Contains(normalized, "unsupported role: developer") ||
		strings.Contains(normalized, "unsupported role 'developer'") ||
		strings.Contains(normalized, `unsupported role "developer"`) ||
		strings.Contains(normalized, "developer role is not supported") ||
		strings.Contains(normalized, "role developer is not supported") ||
		strings.Contains(normalized, "role 'developer' is not supported") ||
		strings.Contains(normalized, `role "developer" is not supported`) ||
		strings.Contains(normalized, "invalid role: developer") ||
		strings.Contains(normalized, "invalid role 'developer'") ||
		strings.Contains(normalized, `invalid role "developer"`) ||
		strings.Contains(normalized, "role developer is invalid") ||
		strings.Contains(normalized, "role 'developer' is invalid") ||
		strings.Contains(normalized, `role "developer" is invalid`)
}
