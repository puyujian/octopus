package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/price"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/model").
		Use(middleware.Auth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listLLM),
		).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createLLM),
		).
		AddRoute(
			router.NewRoute("/channel", http.MethodGet).
				Handle(listLLMByChannel),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateLLM),
		).
		AddRoute(
			router.NewRoute("/delete", http.MethodPost).
				Handle(deleteLLM),
		).
		AddRoute(
			router.NewRoute("/update-price", http.MethodPost).
				Handle(updateLLMPrice),
		).
		AddRoute(
			router.NewRoute("/last-update-time", http.MethodGet).
				Handle(getLastUpdateTime),
		)
	router.NewGroupRouter("/v1").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/models", http.MethodGet).
				Handle(getModelList),
		).
		AddRoute(
			router.NewRoute("/models/*model", http.MethodGet).
				Handle(getModel),
		)
}

func availableModelNames(c *gin.Context) ([]string, error) {
	models, err := op.GroupListModel(c.Request.Context())
	if err != nil {
		return nil, err
	}
	if supported := c.GetString("supported_models"); supported != "" {
		allowed := make(map[string]bool)
		for _, name := range strings.Split(supported, ",") {
			allowed[strings.TrimSpace(name)] = true
		}
		filtered := make([]string, 0, len(models))
		for _, name := range models {
			if allowed[name] {
				filtered = append(filtered, name)
			}
		}
		models = filtered
	}
	return models, nil
}

func newOpenAIModel(name string, metadata price.ModelMetadata, found bool, modelPrice *model.LLMPrice) model.OpenAIModel {
	result := model.OpenAIModel{ID: name, Object: "model", Created: 1763395200, OwnedBy: "octopus"}
	if found {
		result.Name = metadata.Name
		result.Description = metadata.Description
		if metadata.ContextLength > 0 {
			result.ContextLength = metadata.ContextLength
			result.TopProvider = &model.OpenAIModelTopProvider{ContextLength: metadata.ContextLength}
		}
		if metadata.MaxOutputTokens > 0 {
			result.MaxOutputTokens = metadata.MaxOutputTokens
			if result.TopProvider == nil {
				result.TopProvider = &model.OpenAIModelTopProvider{}
			}
			result.TopProvider.MaxCompletionTokens = metadata.MaxOutputTokens
		}
		if len(metadata.InputModalities) > 0 || len(metadata.OutputModalities) > 0 {
			result.Architecture = &model.OpenAIModelArchitecture{
				InputModalities: metadata.InputModalities, OutputModalities: metadata.OutputModalities,
			}
		}
		result.Reasoning = metadata.Reasoning
		result.ReasoningOptions = metadata.ReasoningOptions
	}
	if modelPrice != nil && *modelPrice != (model.LLMPrice{}) {
		result.Pricing = &model.OpenAIModelPricing{
			Prompt:          strconv.FormatFloat(modelPrice.Input/1e6, 'f', -1, 64),
			Completion:      strconv.FormatFloat(modelPrice.Output/1e6, 'f', -1, 64),
			InputCacheRead:  strconv.FormatFloat(modelPrice.CacheRead/1e6, 'f', -1, 64),
			InputCacheWrite: strconv.FormatFloat(modelPrice.CacheWrite/1e6, 'f', -1, 64),
		}
	}
	return result
}

func modelDetails(name string) (price.ModelMetadata, bool, *model.LLMPrice) {
	metadata, found := price.GetModelMetadata(name)
	return metadata, found, price.GetLLMPrice(name)
}

func newAnthropicModel(name string, metadata price.ModelMetadata, found bool) model.AnthropicModel {
	result := model.AnthropicModel{ID: name, CreatedAt: "2024-01-01T00:00:00Z", DisplayName: name, Type: "model"}
	if found {
		if metadata.Name != "" {
			result.DisplayName = metadata.Name
		}
		if metadata.MaxInputTokens > 0 {
			result.MaxInputTokens = &metadata.MaxInputTokens
		} else if metadata.ContextLength > 0 {
			result.MaxInputTokens = &metadata.ContextLength
		}
		if metadata.MaxOutputTokens > 0 {
			result.MaxTokens = &metadata.MaxOutputTokens
		}
		result.Reasoning = metadata.Reasoning
		result.ReasoningOptions = metadata.ReasoningOptions
	}
	return result
}

type etaReasoningMetadata struct {
	SupportedEfforts []string `json:"supported_efforts,omitempty"`
	Mandatory        bool     `json:"mandatory"`
}

type etaOpenAIModel struct {
	model.OpenAIModel
	Reasoning any `json:"reasoning,omitempty"`
}

type etaAnthropicModel struct {
	model.AnthropicModel
	Capabilities gin.H `json:"capabilities,omitempty"`
}

func etaReasoningOptions(reasoning *bool, options []model.ModelReasoningOption) (etaReasoningMetadata, bool, bool) {
	if reasoning == nil || !*reasoning {
		return etaReasoningMetadata{}, false, false
	}
	metadata := etaReasoningMetadata{}
	canDisable := false
	for _, option := range options {
		switch option.Type {
		case "effort":
			metadata.SupportedEfforts = append(metadata.SupportedEfforts, option.Values...)
		case "toggle":
			canDisable = true
		}
	}
	if len(metadata.SupportedEfforts) == 0 && !canDisable {
		return etaReasoningMetadata{}, false, false
	}
	metadata.Mandatory = !canDisable
	return metadata, canDisable, true
}

func newEtaOpenAIModel(result model.OpenAIModel) etaOpenAIModel {
	response := etaOpenAIModel{OpenAIModel: result, Reasoning: result.Reasoning}
	if metadata, _, ok := etaReasoningOptions(result.Reasoning, result.ReasoningOptions); ok {
		response.Reasoning = metadata
	}
	return response
}

func newEtaAnthropicModel(result model.AnthropicModel) etaAnthropicModel {
	response := etaAnthropicModel{AnthropicModel: result}
	metadata, canDisable, ok := etaReasoningOptions(result.Reasoning, result.ReasoningOptions)
	if !ok {
		return response
	}
	capabilities := gin.H{}
	if len(metadata.SupportedEfforts) > 0 {
		efforts := gin.H{"supported": true}
		for _, effort := range metadata.SupportedEfforts {
			efforts[effort] = gin.H{"supported": true}
		}
		capabilities["effort"] = efforts
	}
	if canDisable {
		capabilities["thinking"] = gin.H{"supported": true}
	}
	response.Capabilities = capabilities
	return response
}

func isEtaModelsClient(c *gin.Context) bool {
	userAgent := c.GetHeader("User-Agent")
	return strings.EqualFold(userAgent, "Eta") || strings.HasPrefix(strings.ToLower(userAgent), "eta/") || strings.EqualFold(c.GetHeader("X-Octopus-Model-Format"), "eta")
}

func getModelList(c *gin.Context) {
	models, err := availableModelNames(c)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}

	if c.GetString("request_type") == "anthropic" {
		anthropicModels := make([]any, 0, len(models))
		for _, name := range models {
			metadata, found, _ := modelDetails(name)
			result := newAnthropicModel(name, metadata, found)
			if isEtaModelsClient(c) {
				anthropicModels = append(anthropicModels, newEtaAnthropicModel(result))
			} else {
				anthropicModels = append(anthropicModels, result)
			}
		}
		response := gin.H{
			"data":     anthropicModels,
			"has_more": false,
		}
		if len(models) > 0 {
			response["first_id"] = models[0]
			response["last_id"] = models[len(models)-1]
		}
		c.JSON(200, response)
	} else {
		openAIModels := make([]any, 0, len(models))
		for _, name := range models {
			metadata, found, modelPrice := modelDetails(name)
			result := newOpenAIModel(name, metadata, found, modelPrice)
			if isEtaModelsClient(c) {
				openAIModels = append(openAIModels, newEtaOpenAIModel(result))
			} else {
				openAIModels = append(openAIModels, result)
			}
		}
		c.JSON(200, gin.H{
			"success": true,
			"data":    openAIModels,
			"object":  "list",
		})
	}
}

func getModel(c *gin.Context) {
	name := strings.TrimPrefix(c.Param("model"), "/")
	models, err := availableModelNames(c)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	for _, available := range models {
		if available != name {
			continue
		}
		metadata, found, modelPrice := modelDetails(name)
		if c.GetString("request_type") == "anthropic" {
			result := newAnthropicModel(name, metadata, found)
			if isEtaModelsClient(c) {
				c.JSON(http.StatusOK, newEtaAnthropicModel(result))
			} else {
				c.JSON(http.StatusOK, result)
			}
		} else {
			result := newOpenAIModel(name, metadata, found, modelPrice)
			if isEtaModelsClient(c) {
				c.JSON(http.StatusOK, newEtaOpenAIModel(result))
			} else {
				c.JSON(http.StatusOK, result)
			}
		}
		return
	}
	resp.NotFound(c)
}

func listLLM(c *gin.Context) {
	models, err := op.LLMList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, models)
}

func listLLMByChannel(c *gin.Context) {
	channels, err := op.ChannelLLMList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, channels)
}

func createLLM(c *gin.Context) {
	var model model.LLMInfo
	if err := c.ShouldBindJSON(&model); err != nil {
		resp.InvalidJSON(c)
		return
	}
	if err := op.LLMCreate(model, c.Request.Context()); err != nil {
		resp.ErrorWithAppError(c, http.StatusInternalServerError, modelError(codeModelCreateFailed, "model create failed", err))
		return
	}
	resp.Success(c, model)
}

func updateLLM(c *gin.Context) {
	var model model.LLMInfo
	if err := c.ShouldBindJSON(&model); err != nil {
		resp.InvalidJSON(c)
		return
	}
	if err := op.LLMUpdate(model, c.Request.Context()); err != nil {
		resp.ErrorWithAppError(c, http.StatusInternalServerError, modelError(codeModelUpdateFailed, "model update failed", err))
		return
	}
	resp.Success(c, model)
}

func deleteLLM(c *gin.Context) {
	var req struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.InvalidJSON(c)
		return
	}
	if err := op.LLMDelete(req.Name, c.Request.Context()); err != nil {
		resp.ErrorWithAppError(c, http.StatusInternalServerError, modelError(codeModelPriceDeleteFailed, "model price delete failed", err))
		return
	}
	resp.Success(c, nil)
}

func updateLLMPrice(c *gin.Context) {
	err := price.UpdateLLMPrice(c.Request.Context())
	if err != nil {
		resp.ErrorWithAppError(c, http.StatusInternalServerError, modelError(codeModelPriceUpdateFailed, "model price update failed", err))
		return
	}
	resp.Success(c, nil)
}

func getLastUpdateTime(c *gin.Context) {
	time := price.GetLastUpdateTime()
	resp.Success(c, time)
}
