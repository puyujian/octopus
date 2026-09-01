package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func TestUpstreamRejectsDeveloperRole(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       bool
	}{
		{
			name:       "provider role list",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"message":"developer is not one of ['system', 'assistant', 'user', 'tool']"}}`,
			want:       true,
		},
		{
			name:       "unsupported value",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"message":"Unsupported value: 'developer' for role"}}`,
			want:       true,
		},
		{
			name:       "unrelated bad request",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"message":"developer quota exhausted"}}`,
			want:       false,
		},
		{
			name:       "unrelated invalid role",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"message":"invalid role: user; see developer documentation"}}`,
			want:       false,
		},
		{
			name:       "wrong status",
			statusCode: http.StatusTooManyRequests,
			body:       `{"error":{"message":"developer is not one of ['system']"}}`,
			want:       false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := upstreamRejectsDeveloperRole(test.statusCode, []byte(test.body)); got != test.want {
				t.Fatalf("upstreamRejectsDeveloperRole() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestRequestWithDeveloperRoleDowngradedDoesNotMutateOriginal(t *testing.T) {
	request := &transformerModel.InternalLLMRequest{Messages: []transformerModel.Message{
		{Role: "developer"},
		{Role: "user"},
	}}

	downgraded, changed := requestWithDeveloperRoleDowngraded(request)
	if !changed {
		t.Fatal("expected developer role downgrade")
	}
	if downgraded == request || downgraded.Messages[0].Role != "system" {
		t.Fatalf("expected a cloned request with system role, got %#v", downgraded)
	}
	if request.Messages[0].Role != "developer" {
		t.Fatalf("original request was mutated: %#v", request.Messages)
	}
}

func TestHandlerDowngradesRejectedDeveloperRoleAndCachesCapability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetDeveloperRoleCompatibilityCache()
	t.Cleanup(resetDeveloperRoleCompatibilityCache)
	ctx := setupRelayTestDB(t)

	var capturedRoles [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		roles := make([]string, len(payload.Messages))
		for index := range payload.Messages {
			roles[index] = payload.Messages[index].Role
		}
		capturedRoles = append(capturedRoles, roles)

		if len(capturedRoles) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_parameter_error","message":"developer is not one of ['system', 'assistant', 'user', 'tool', 'function']","type":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_compat","object":"chat.completion","created":1,"model":"ZhipuAI/GLM-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	modelName := "ZhipuAI/GLM-5.2"
	groupName, channelID := createDeveloperRoleCompatibilityRoute(t, ctx, server.URL, "cached", modelName)

	first := runDeveloperRoleResponsesRequest(t, groupName)
	if first.Code != http.StatusOK {
		t.Fatalf("expected compatibility retry to succeed, got status %d body %s", first.Code, first.Body.String())
	}
	second := runDeveloperRoleResponsesRequest(t, groupName)
	if second.Code != http.StatusOK {
		t.Fatalf("expected cached downgrade request to succeed, got status %d body %s", second.Code, second.Body.String())
	}

	wantRoles := [][]string{
		{"developer", "user"},
		{"system", "user"},
		{"system", "user"},
	}
	if len(capturedRoles) != len(wantRoles) {
		t.Fatalf("expected three upstream requests, got roles %#v", capturedRoles)
	}
	for index := range wantRoles {
		if strings.Join(capturedRoles[index], ",") != strings.Join(wantRoles[index], ",") {
			t.Fatalf("upstream request %d roles = %#v, want %#v", index+1, capturedRoles[index], wantRoles[index])
		}
	}
	if !channelModelRequiresSystemRole(channelID, modelName) {
		t.Fatal("expected channel/model compatibility result to be cached")
	}
}

func TestHandlerDoesNotDowngradeForUnrelatedBadRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetDeveloperRoleCompatibilityCache()
	t.Cleanup(resetDeveloperRoleCompatibilityCache)
	ctx := setupRelayTestDB(t)

	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"developer quota exhausted"}}`)
	}))
	defer server.Close()

	modelName := "unrelated-error-model"
	groupName, channelID := createDeveloperRoleCompatibilityRoute(t, ctx, server.URL, "unrelated", modelName)
	recorder := runDeveloperRoleChatRequest(t, groupName)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected unrelated upstream 400 to pass through, got status %d body %s", recorder.Code, recorder.Body.String())
	}
	if hits != 1 {
		t.Fatalf("expected no compatibility retry, got %d upstream requests", hits)
	}
	if channelModelRequiresSystemRole(channelID, modelName) {
		t.Fatal("unrelated error must not populate the compatibility cache")
	}
}

func TestHandlerDeveloperRoleDowngradeRetriesOnlyOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetDeveloperRoleCompatibilityCache()
	t.Cleanup(resetDeveloperRoleCompatibilityCache)
	ctx := setupRelayTestDB(t)

	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"developer is not one of ['system', 'assistant', 'user', 'tool']"}}`)
	}))
	defer server.Close()

	groupName, _ := createDeveloperRoleCompatibilityRoute(t, ctx, server.URL, "once", "retry-once-model")
	recorder := runDeveloperRoleChatRequest(t, groupName)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected second upstream 400 to be returned, got status %d body %s", recorder.Code, recorder.Body.String())
	}
	if hits != 2 {
		t.Fatalf("expected exactly one compatibility retry, got %d upstream requests", hits)
	}
}

func createDeveloperRoleCompatibilityRoute(t *testing.T, ctx context.Context, serverURL, suffix, modelName string) (string, int) {
	t.Helper()
	channel := &dbmodel.Channel{
		Name:     "developer-role-compat-" + suffix,
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: serverURL + "/v1"}},
		Model:    modelName,
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}

	group := &dbmodel.Group{Name: "developer-role-compat-group-" + suffix, Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID: group.ID, ChannelID: channel.ID, ModelName: modelName, Priority: 1, Weight: 1,
	}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}
	return group.Name, channel.ID
}

func runDeveloperRoleResponsesRequest(t *testing.T, groupName string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + groupName + `","input":[` +
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"follow the rules"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}` +
		`]}`
	return runDeveloperRoleRequest(t, inbound.InboundTypeOpenAIResponse, "/v1/responses", body)
}

func runDeveloperRoleChatRequest(t *testing.T, groupName string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + groupName + `","messages":[` +
		`{"role":"developer","content":"follow the rules"},` +
		`{"role":"user","content":"hello"}` +
		`]}`
	return runDeveloperRoleRequest(t, inbound.InboundTypeOpenAIChat, "/v1/chat/completions", body)
}

func runDeveloperRoleRequest(t *testing.T, inboundType inbound.InboundType, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("api_key_id", 77)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	Handler(inboundType, c)
	return recorder
}
