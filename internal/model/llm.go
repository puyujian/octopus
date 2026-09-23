package model

type LLMPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

type LLMInfo struct {
	Name string `json:"name" gorm:"primaryKey;not null"`
	LLMPrice
}

type LLMChannel struct {
	Name            string `json:"name"`
	Enabled         bool   `json:"enabled"`
	ChannelID       int    `json:"channel_id"`
	ChannelName     string `json:"channel_name"`
	SiteID          *int   `json:"site_id,omitempty"`
	SiteAccountID   *int   `json:"site_account_id,omitempty"`
	SiteGroupKey    string `json:"site_group_key,omitempty"`
	SiteGroupName   string `json:"site_group_name,omitempty"`
	SiteName        string `json:"site_name,omitempty"`
	SiteAccountName string `json:"site_account_name,omitempty"`
	EndpointType    string `json:"endpoint_type,omitempty"`
}

type GeminiModel struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

type GeminiModelList struct {
	Models        []GeminiModel `json:"models"`
	NextPageToken string        `json:"nextPageToken"`
}

type OpenAIModel struct {
	ID               string                   `json:"id"`
	Object           string                   `json:"object"`
	Created          int                      `json:"created"`
	OwnedBy          string                   `json:"owned_by"`
	Name             string                   `json:"name,omitempty"`
	Description      string                   `json:"description,omitempty"`
	ContextLength    int                      `json:"context_length,omitempty"`
	MaxOutputTokens  int                      `json:"max_output_tokens,omitempty"`
	TopProvider      *OpenAIModelTopProvider  `json:"top_provider,omitempty"`
	Architecture     *OpenAIModelArchitecture `json:"architecture,omitempty"`
	Pricing          *OpenAIModelPricing      `json:"pricing,omitempty"`
	Reasoning        *bool                    `json:"reasoning,omitempty"`
	ReasoningOptions []ModelReasoningOption   `json:"reasoning_options,omitempty"`
}

type ModelReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values,omitempty"`
}

type OpenAIModelTopProvider struct {
	ContextLength       int `json:"context_length,omitempty"`
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
}

type OpenAIModelArchitecture struct {
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
}

type OpenAIModelPricing struct {
	Prompt          string `json:"prompt"`
	Completion      string `json:"completion"`
	InputCacheRead  string `json:"input_cache_read,omitempty"`
	InputCacheWrite string `json:"input_cache_write,omitempty"`
}

type OpenAIModelList struct {
	Object string        `json:"object"`
	Data   []OpenAIModel `json:"data"`
}
type AnthropicModel struct {
	ID               string                 `json:"id"`
	CreatedAt        string                 `json:"created_at"`
	DisplayName      string                 `json:"display_name"`
	Type             string                 `json:"type"`
	MaxInputTokens   *int                   `json:"max_input_tokens,omitempty"`
	MaxTokens        *int                   `json:"max_tokens,omitempty"`
	Reasoning        *bool                  `json:"reasoning,omitempty"`
	ReasoningOptions []ModelReasoningOption `json:"reasoning_options,omitempty"`
}

type AnthropicModelList struct {
	Data    []AnthropicModel `json:"data"`
	FirstID string           `json:"first_id"`
	HasMore bool             `json:"has_more"`
	LastID  string           `json:"last_id"`
}
