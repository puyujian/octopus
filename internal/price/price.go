package price

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/client"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/utils/log"
)

const llmPriceUrl = "https://models.dev/api.json"

var Provider = []string{
	"openai",     // GPT 系列
	"anthropic",  // Claude 系列
	"google",     // Gemini 系列
	"deepseek",   // DeepSeek 系列
	"xai",        // Grok 系列
	"alibaba",    // Qwen 系列
	"zhipuai",    // GLM 系列
	"minimax",    // MiniMax 系列
	"moonshotai", // Kimi/Moonshot
	"v0",         // v0 系列
}

var lastUpdateTime time.Time

type ModelMetadata struct {
	Name             string
	Description      string
	ContextLength    int
	MaxInputTokens   int
	MaxOutputTokens  int
	InputModalities  []string
	OutputModalities []string
	Reasoning        *bool
	ReasoningOptions []model.ModelReasoningOption
}

var modelMetadata = make(map[string]ModelMetadata)

func UpdateLLMPrice(ctx context.Context) error {
	log.Debugf("update LLM price task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("update LLM price task finished, update time: %s", time.Since(startTime))
	}()
	client, err := client.GetHTTPClientSystemProxy(false)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, llmPriceUrl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch LLM info: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	if err := updateModelCatalog(body); err != nil {
		return err
	}
	lastUpdateTime = time.Now()
	return nil
}

func updateModelCatalog(body []byte) error {
	var rawPrice map[string]struct {
		Models map[string]struct {
			ID          string         `json:"id"`
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Cost        model.LLMPrice `json:"cost"`
			Limit       struct {
				Context int `json:"context"`
				Input   int `json:"input"`
				Output  int `json:"output"`
			} `json:"limit"`
			Modalities struct {
				Input  []string `json:"input"`
				Output []string `json:"output"`
			} `json:"modalities"`
			Reasoning        *bool                        `json:"reasoning"`
			ReasoningOptions []model.ModelReasoningOption `json:"reasoning_options"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &rawPrice); err != nil {
		return fmt.Errorf("failed to parse LLM info: %w", err)
	}
	llmPriceLock.Lock()
	metadata := make(map[string]ModelMetadata)
	for _, provider := range Provider {
		for _, model := range rawPrice[provider].Models {
			model.ID = strings.ToLower(model.ID)
			if model.ID == "" {
				continue
			}
			llmPrice[model.ID] = model.Cost
			metadata[model.ID] = ModelMetadata{
				Name:             model.Name,
				Description:      model.Description,
				ContextLength:    model.Limit.Context,
				MaxInputTokens:   model.Limit.Input,
				MaxOutputTokens:  model.Limit.Output,
				InputModalities:  model.Modalities.Input,
				OutputModalities: model.Modalities.Output,
				Reasoning:        model.Reasoning,
				ReasoningOptions: model.ReasoningOptions,
			}
		}
	}
	modelMetadata = metadata
	llmPriceLock.Unlock()
	return nil
}

func GetModelMetadata(modelName string) (ModelMetadata, bool) {
	llmPriceLock.RLock()
	defer llmPriceLock.RUnlock()
	metadata, ok := modelMetadata[strings.ToLower(modelName)]
	return metadata, ok
}

func GetLastUpdateTime() time.Time {
	return lastUpdateTime
}

func GetLLMPrice(modelName string) *model.LLMPrice {
	modelName = strings.ToLower(modelName)
	price, err := op.LLMGet(modelName)
	if err == nil {
		return &price
	}
	llmPriceLock.RLock()
	defer llmPriceLock.RUnlock()
	price, ok := llmPrice[modelName]
	if !ok {
		return nil
	}
	return &price
}
