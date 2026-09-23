package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/price"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func TestModelMetadataSerialization(t *testing.T) {
	reasoning := true
	metadata := price.ModelMetadata{
		Name: "Example", Description: "Description", ContextLength: 128000,
		MaxOutputTokens: 16384, InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"},
		Reasoning: &reasoning, ReasoningOptions: []model.ModelReasoningOption{{Type: "effort", Values: []string{"low", "high", "max"}}},
	}
	cost := &model.LLMPrice{Input: 2.5, Output: 10, CacheRead: 1.25}
	openAIModel := newOpenAIModel("test-model", metadata, true, cost)
	if openAIModel.Pricing == nil || openAIModel.Pricing.Prompt != "0.0000025" || openAIModel.Pricing.Completion != "0.00001" || openAIModel.Pricing.InputCacheRead != "0.00000125" {
		t.Fatalf("incorrect price conversion: %+v", openAIModel.Pricing)
	}
	if openAIModel.ContextLength != 128000 || openAIModel.TopProvider.MaxCompletionTokens != 16384 || len(openAIModel.Architecture.InputModalities) != 2 || openAIModel.Reasoning == nil || !*openAIModel.Reasoning || len(openAIModel.ReasoningOptions) != 1 {
		t.Fatalf("missing model metadata: %+v", openAIModel)
	}
	if anthropicModel := newAnthropicModel("test-model", metadata, true); anthropicModel.DisplayName != "Example" || *anthropicModel.MaxInputTokens != 128000 || *anthropicModel.MaxTokens != 16384 || anthropicModel.Reasoning == nil || !*anthropicModel.Reasoning {
		t.Fatalf("missing Anthropic model metadata: %+v", anthropicModel)
	}
	metadata.MaxInputTokens = 111616
	if anthropicModel := newAnthropicModel("test-model", metadata, true); *anthropicModel.MaxInputTokens != 111616 {
		t.Fatalf("specific input limit should take priority: %+v", anthropicModel)
	}

	unknownModel, err := json.Marshal(newOpenAIModel("unknown", price.ModelMetadata{}, false, nil))
	if err != nil {
		t.Fatal(err)
	}
	var unknownFields map[string]any
	if err := json.Unmarshal(unknownModel, &unknownFields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"context_length", "max_output_tokens", "pricing", "architecture", "top_provider", "reasoning", "reasoning_options"} {
		if _, exists := unknownFields[field]; exists {
			t.Fatalf("unknown model should not claim %s: %s", field, unknownModel)
		}
	}
}

func TestEtaReasoningMetadataCompatibility(t *testing.T) {
	reasoning := true
	for _, testCase := range []struct {
		name      string
		options   []model.ModelReasoningOption
		mandatory bool
		thinking  bool
	}{
		{name: "glm-5.3", options: []model.ModelReasoningOption{{Type: "effort", Values: []string{"low", "high", "max"}}}, mandatory: true},
		{name: "deepseek-v4-flash", options: []model.ModelReasoningOption{{Type: "toggle"}, {Type: "effort", Values: []string{"low", "high", "max"}}}, thinking: true},
	} {
		metadata := price.ModelMetadata{ContextLength: 1000000, Reasoning: &reasoning, ReasoningOptions: testCase.options}
		standardJSON, err := json.Marshal(newOpenAIModel(testCase.name, metadata, true, nil))
		if err != nil {
			t.Fatal(err)
		}
		var standard map[string]any
		if err := json.Unmarshal(standardJSON, &standard); err != nil || standard["reasoning"] != true {
			t.Fatalf("standard clients must retain a boolean reasoning value: %s, %v", standardJSON, err)
		}

		etaJSON, err := json.Marshal(newEtaOpenAIModel(newOpenAIModel(testCase.name, metadata, true, nil)))
		if err != nil {
			t.Fatal(err)
		}
		var eta map[string]any
		if err := json.Unmarshal(etaJSON, &eta); err != nil {
			t.Fatal(err)
		}
		capabilities, ok := eta["reasoning"].(map[string]any)
		if !ok || capabilities["mandatory"] != testCase.mandatory || eta["context_length"] != float64(1000000) {
			t.Fatalf("ETA should receive the reasoning object and context for %s: %s", testCase.name, etaJSON)
		}
		efforts, ok := capabilities["supported_efforts"].([]any)
		if !ok || len(efforts) != 3 || efforts[2] != "max" {
			t.Fatalf("ETA should receive three selectable efforts for %s: %s", testCase.name, etaJSON)
		}

		anthropicJSON, err := json.Marshal(newEtaAnthropicModel(newAnthropicModel(testCase.name, metadata, true)))
		if err != nil {
			t.Fatal(err)
		}
		var anthropic map[string]any
		if err := json.Unmarshal(anthropicJSON, &anthropic); err != nil {
			t.Fatal(err)
		}
		anthropicCapabilities, ok := anthropic["capabilities"].(map[string]any)
		if !ok || (anthropicCapabilities["thinking"] != nil) != testCase.thinking || anthropicCapabilities["effort"] == nil {
			t.Fatalf("ETA Anthropic capabilities incorrect for %s: %s", testCase.name, anthropicJSON)
		}
	}

	requestContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	requestContext.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	requestContext.Request.Header.Set("User-Agent", "Eta")
	if !isEtaModelsClient(requestContext) {
		t.Fatal("ETA requests should receive the ETA model format")
	}
	requestContext.Request.Header.Set("User-Agent", "okhttp/4.12.0")
	if isEtaModelsClient(requestContext) {
		t.Fatal("other OkHttp clients should keep the default format")
	}
	requestContext.Request.Header.Set("X-Octopus-Model-Format", "eta")
	if !isEtaModelsClient(requestContext) {
		t.Fatal("explicit ETA format should support clients without the ETA user agent")
	}
}

func TestModelListAndRetrieveRespectSupportedModels(t *testing.T) {
	if db.GetDB() != nil {
		_ = db.Close()
	}
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "model-list.db"), false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatal(err)
	}

	contextValue := context.Background()
	channel := &model.Channel{
		Name: "model-list-test", Type: outbound.OutboundTypeOpenAIChat,
		Enabled: true, Model: "octopus-model-test,custom/model-test",
	}
	if err := op.ChannelCreate(channel, contextValue); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"octopus-model-test", "custom/model-test", "aggregate-model-test"} {
		group := &model.Group{Name: name, Mode: model.GroupModeRoundRobin}
		if err := op.GroupCreate(group, contextValue); err != nil {
			t.Fatal(err)
		}
		underlying := name
		if name == "aggregate-model-test" {
			underlying = "octopus-model-test"
		}
		if err := op.GroupItemAdd(&model.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: underlying, Weight: 1}, contextValue); err != nil {
			t.Fatal(err)
		}
	}
	if err := op.LLMCreate(model.LLMInfo{Name: "octopus-model-test", LLMPrice: model.LLMPrice{Input: 2, Output: 8}}, contextValue); err != nil {
		t.Fatal(err)
	}
	if err := op.LLMCreate(model.LLMInfo{Name: "aggregate-model-test", LLMPrice: model.LLMPrice{Input: 2, Output: 8}}, contextValue); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(requestContext *gin.Context) {
		requestContext.Set("supported_models", requestContext.GetHeader("X-Test-Supported"))
	})
	engine.GET("/v1/models", getModelList)
	engine.GET("/v1/models/*model", getModel)

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("X-Test-Supported", "octopus-model-test")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("model list returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Data []model.OpenAIModel `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data) != 1 || result.Data[0].ID != "octopus-model-test" || result.Data[0].Pricing == nil || result.Data[0].Pricing.Prompt != "0.000002" {
		t.Fatalf("filtered list or configured price incorrect: %+v", result)
	}

	for _, testCase := range []struct {
		path      string
		allowed   string
		wantCode  int
		wantModel string
	}{
		{path: "/v1/models/octopus-model-test", allowed: "octopus-model-test", wantCode: http.StatusOK, wantModel: "octopus-model-test"},
		{path: "/v1/models/custom/model-test", allowed: "custom/model-test", wantCode: http.StatusOK, wantModel: "custom/model-test"},
		{path: "/v1/models/aggregate-model-test", allowed: "aggregate-model-test", wantCode: http.StatusOK, wantModel: "aggregate-model-test"},
		{path: "/v1/models/custom/model-test", allowed: "octopus-model-test", wantCode: http.StatusNotFound},
		{path: "/v1/models/missing", allowed: "octopus-model-test", wantCode: http.StatusNotFound},
	} {
		request = httptest.NewRequest(http.MethodGet, testCase.path, nil)
		request.Header.Set("X-Test-Supported", testCase.allowed)
		recorder = httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		if recorder.Code != testCase.wantCode {
			t.Fatalf("%s returned %d, want %d: %s", testCase.path, recorder.Code, testCase.wantCode, recorder.Body.String())
		}
		if testCase.wantModel != "" {
			var detail model.OpenAIModel
			if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil || detail.ID != testCase.wantModel {
				t.Fatalf("unexpected model detail for %s: %+v, %v", testCase.path, detail, err)
			}
			if testCase.wantModel == "aggregate-model-test" && (detail.Pricing == nil || detail.Pricing.Prompt != "0.000002" || detail.ContextLength != 0) {
				t.Fatalf("custom group price should be visible without inventing missing catalog data: %+v", detail)
			}
		}
	}
}
