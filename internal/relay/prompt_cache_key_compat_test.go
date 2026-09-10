package relay

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

const promptCacheKeyRejection = "{\"error\":{\"message\":\"Validation: Unsupported parameter(s): `prompt_cache_key`\",\"type\":\"bad_response_status_code\",\"param\":\"\",\"code\":\"bad_response_status_code\"}}"

func resetPromptCacheKeyCompatibility(t *testing.T) {
	t.Helper()
	promptCacheKeyCompatibility.Clear()
	t.Cleanup(promptCacheKeyCompatibility.Clear)
}

func TestHandlerPromptCacheKeyCompatibility(t *testing.T) {
	for _, test := range []struct {
		name         string
		inboundType  inbound.InboundType
		outboundType outbound.OutboundType
		stream       bool
	}{
		{"chat", inbound.InboundTypeOpenAIChat, outbound.OutboundTypeOpenAIChat, false},
		{"responses to chat", inbound.InboundTypeOpenAIResponse, outbound.OutboundTypeOpenAIChat, false},
		{"responses to chat stream", inbound.InboundTypeOpenAIResponse, outbound.OutboundTypeOpenAIChat, true},
		{"responses passthrough", inbound.InboundTypeOpenAIResponse, outbound.OutboundTypeOpenAIResponse, false},
		{"responses passthrough stream", inbound.InboundTypeOpenAIResponse, outbound.OutboundTypeOpenAIResponse, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			resetPromptCacheKeyCompatibility(t)
			ctx := setupRelayTestDB(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if r.ContentLength != int64(len(body)) {
					t.Errorf("Content-Length = %d, body length = %d", r.ContentLength, len(body))
				}
				if string(payload["model"]) != `"compat-model"` || string(payload["stream"]) != fmt.Sprint(test.stream) {
					t.Errorf("routing/stream fields changed: %s", body)
				}
				if test.outboundType == outbound.OutboundTypeOpenAIResponse {
					if r.URL.Path != "/v1/responses" || string(payload["provider_extension"]) != `{"id":"opaque-extension"}` {
						t.Errorf("passthrough data changed: path=%s body=%s", r.URL.Path, body)
					}
				} else if r.URL.Path != "/v1/chat/completions" || len(payload["messages"]) == 0 {
					t.Errorf("chat conversion lost messages: path=%s body=%s", r.URL.Path, body)
				}
				_, hasCacheKey := payload["prompt_cache_key"]
				if calls.Add(1) == 1 {
					if !hasCacheKey {
						t.Error("first request must preserve the cache hint")
					}
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, promptCacheKeyRejection)
					return
				}
				if hasCacheKey {
					t.Error("retry and later requests must omit the rejected cache hint")
				}
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					if test.outboundType == outbound.OutboundTypeOpenAIResponse {
						_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cache\",\"object\":\"response\",\"model\":\"compat-model\",\"created_at\":1,\"output\":[],\"status\":\"in_progress\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_cache\",\"object\":\"response\",\"model\":\"compat-model\",\"created_at\":1,\"output\":[],\"status\":\"completed\"}}\n\n")
					} else {
						_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_cache\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"compat-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl_cache\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"compat-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if test.outboundType == outbound.OutboundTypeOpenAIResponse {
					_, _ = io.WriteString(w, `{"id":"resp_cache","object":"response","created_at":1,"model":"compat-model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
				} else {
					_, _ = io.WriteString(w, `{"id":"chatcmpl_cache","object":"chat.completion","created":1,"model":"compat-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
				}
			}))
			defer server.Close()

			// An override must not reintroduce the field on the cached path.
			override := `{"prompt_cache_key":"channel-cache-key"}`
			channel := &dbmodel.Channel{
				Name: "cache-compat", Type: test.outboundType, Enabled: true,
				BaseUrls: []dbmodel.BaseUrl{{URL: server.URL + "/v1"}}, Model: "compat-model",
				Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
			}
			if test.inboundType == inbound.InboundTypeOpenAIChat {
				channel.ParamOverride = &override
			}
			if err := op.ChannelCreate(channel, ctx); err != nil {
				t.Fatal(err)
			}
			group := &dbmodel.Group{Name: "cache-compat-group", Mode: dbmodel.GroupModeFailover, FirstTokenTimeOut: 5}
			if err := op.GroupCreate(group, ctx); err != nil {
				t.Fatal(err)
			}
			if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: "compat-model", Priority: 1, Weight: 1}, ctx); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				path := "/v1/responses"
				body := fmt.Sprintf(`{"model":"cache-compat-group","input":"hello","prompt_cache_key":"client-cache-key","stream":%t,"provider_extension":{"id":"opaque-extension"}}`, test.stream)
				if test.inboundType == inbound.InboundTypeOpenAIChat {
					path = "/v1/chat/completions"
					body = `{"model":"cache-compat-group","messages":[{"role":"user","content":"hello"}],"prompt_cache_key":"client-cache-key","stream":false}`
				}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				Handler(test.inboundType, c)
				if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ok") {
					t.Fatalf("request %d: status=%d body=%s", attempt+1, recorder.Code, recorder.Body.String())
				}
			}
			if got := calls.Load(); got != 3 {
				t.Fatalf("expected retry then cached success (3 upstream calls), got %d", got)
			}
		})
	}
}

func TestSendRequestPromptCacheKeyCompatibilityGuards(t *testing.T) {
	setupRelayTestDB(t)
	for _, test := range []struct {
		name   string
		status int
		body   string
		input  string
		calls  int32
	}{
		{"supported", 200, `{}`, `{"prompt_cache_key":"cache"}`, 1},
		{"unrelated validation", 400, `{"error":{"message":"Invalid value for prompt_cache_key"}}`, `{"prompt_cache_key":"cache"}`, 1},
		{"authentication", 401, promptCacheKeyRejection, `{"prompt_cache_key":"cache"}`, 1},
		{"rate limit", 429, promptCacheKeyRejection, `{"prompt_cache_key":"cache"}`, 1},
		{"parameter absent", 400, promptCacheKeyRejection, `{}`, 1},
		{"bounded retry", 400, promptCacheKeyRejection, `{"prompt_cache_key":"cache"}`, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetPromptCacheKeyCompatibility(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(test.input))
			req.GetBody = nil // Exercise adapters with a non-replayable initial body.
			internal := &transformerModel.InternalLLMRequest{Model: "compat-model"}
			ra := &relayAttempt{
				relayRequest: &relayRequest{internalRequest: internal, metrics: NewRelayMetrics(1, internal.Model, nil, internal)},
				channel:      &dbmodel.Channel{ID: 1, Type: outbound.OutboundTypeOpenAIChat},
			}
			response, err := ra.sendRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != test.status || string(body) != test.body || response.Header.Get("Retry-After") != "7" {
				t.Fatalf("response changed: status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
			}
			if got := calls.Load(); got != test.calls {
				t.Fatalf("upstream calls = %d, want %d", got, test.calls)
			}
		})
	}
}

func TestSendRequestPromptCacheKeyCompatibilityIsolation(t *testing.T) {
	setupRelayTestDB(t)
	resetPromptCacheKeyCompatibility(t)
	var calls atomic.Int32
	var cacheKeyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if string(body["provider_extension"]) != `{"id":9007199254740993,"prompt_cache_key":"nested"}` {
			t.Errorf("compatibility changed unrelated or nested fields: %s", body["provider_extension"])
		}
		if _, present := body["prompt_cache_key"]; present {
			cacheKeyCalls.Add(1)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, promptCacheKeyRejection)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	for _, test := range []struct {
		channelID int
		model     string
		path      string
	}{
		{1, "first-model", "/v1/chat/completions"},
		{1, "first-model", "/v1/chat/completions"},
		{2, "first-model", "/v1/chat/completions"},
		{1, "second-model", "/v1/chat/completions"},
		{1, "first-model", "/v1/responses"},
		{1, "first-model", "/changed/v1/chat/completions"},
	} {
		internal := &transformerModel.InternalLLMRequest{Model: test.model}
		ra := &relayAttempt{
			relayRequest: &relayRequest{internalRequest: internal, metrics: NewRelayMetrics(1, internal.Model, nil, internal)},
			channel:      &dbmodel.Channel{ID: test.channelID, Type: outbound.OutboundTypeOpenAIChat},
		}
		req, _ := http.NewRequest(http.MethodPost, server.URL+test.path, strings.NewReader(`{"prompt_cache_key":"cache","provider_extension":{"id":9007199254740993,"prompt_cache_key":"nested"}}`))
		response, err := ra.sendRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	if calls.Load() != 7 || cacheKeyCalls.Load() != 5 {
		t.Fatalf("compatibility leaked to other channel/model/endpoint: calls=%d cache-key calls=%d", calls.Load(), cacheKeyCalls.Load())
	}
}
