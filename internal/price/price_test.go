package price

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

func TestUpdateModelCatalogLoadsAndRefreshesMetadata(t *testing.T) {
	modelNames := []string{"octopus-metadata-test", "glm-5.3", "deepseek-v4-flash"}
	llmPriceLock.Lock()
	previousMetadata := modelMetadata
	previousPrices := make(map[string]struct {
		price model.LLMPrice
		found bool
	}, len(modelNames))
	for _, name := range modelNames {
		cost, found := llmPrice[name]
		previousPrices[name] = struct {
			price model.LLMPrice
			found bool
		}{cost, found}
	}
	llmPriceLock.Unlock()
	t.Cleanup(func() {
		llmPriceLock.Lock()
		modelMetadata = previousMetadata
		for name, previous := range previousPrices {
			if previous.found {
				llmPrice[name] = previous.price
			} else {
				delete(llmPrice, name)
			}
		}
		llmPriceLock.Unlock()
	})

	data := []byte(`{"openai":{"models":{"model":{"id":"Octopus-Metadata-Test","name":"Metadata test","description":"Catalog description","cost":{"input":2.5,"output":10},"limit":{"context":128000,"input":111616,"output":16384},"modalities":{"input":["text","image"],"output":["text"]}}}},"zhipuai":{"models":{"glm-5.3":{"id":"glm-5.3","reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high","max"]}],"limit":{"context":1000000,"output":131072}}}},"deepseek":{"models":{"deepseek-v4-flash":{"id":"deepseek-v4-flash","reasoning":true,"reasoning_options":[{"type":"toggle"},{"type":"effort","values":["low","high","max"]}],"limit":{"context":1000000,"output":384000}}}}}`)
	if err := updateModelCatalog(data); err != nil {
		t.Fatal(err)
	}
	metadata, ok := GetModelMetadata("OCTOPUS-METADATA-TEST")
	if !ok || metadata.Name != "Metadata test" || metadata.Description != "Catalog description" || metadata.ContextLength != 128000 || metadata.MaxInputTokens != 111616 || metadata.MaxOutputTokens != 16384 || len(metadata.InputModalities) != 2 {
		t.Fatalf("unexpected model metadata: %+v, found=%v", metadata, ok)
	}
	for _, testCase := range []struct {
		name        string
		output      int
		optionCount int
	}{
		{name: "glm-5.3", output: 131072, optionCount: 1},
		{name: "deepseek-v4-flash", output: 384000, optionCount: 2},
	} {
		metadata, ok := GetModelMetadata(testCase.name)
		if !ok || metadata.ContextLength != 1000000 || metadata.MaxOutputTokens != testCase.output || metadata.Reasoning == nil || !*metadata.Reasoning || len(metadata.ReasoningOptions) != testCase.optionCount || metadata.ReasoningOptions[testCase.optionCount-1].Values[2] != "max" {
			t.Fatalf("missing reasoning metadata for %s: %+v, found=%v", testCase.name, metadata, ok)
		}
	}
	llmPriceLock.RLock()
	cost := llmPrice["octopus-metadata-test"]
	llmPriceLock.RUnlock()
	if cost.Input != 2.5 || cost.Output != 10 {
		t.Fatalf("unexpected catalog price: %+v", cost)
	}
	if err := updateModelCatalog([]byte(`{"openai":{"models":{}}}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := GetModelMetadata("octopus-metadata-test"); ok {
		t.Fatal("removed metadata must not remain in the catalog")
	}
	if err := updateModelCatalog([]byte(`{invalid`)); err == nil {
		t.Fatal("invalid catalog should fail")
	}
}
