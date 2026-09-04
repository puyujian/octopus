package op

import (
	"sync"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestChannelModelRouteLearnMergesConcurrentModelsAndHonorsOverride(t *testing.T) {
	ctx := setupSiteOpTestDB(t)
	if err := InitCache(); err != nil {
		t.Fatal(err)
	}
	channel := &model.Channel{
		Name:    "auto-route-learning",
		Type:    outbound.OutboundTypeAuto,
		Enabled: true,
		Model:   "model-a,model-b,model-c",
		ModelRoutes: model.ChannelModelRoutes{
			FallbackType: outbound.OutboundTypeOpenAIChat,
			Overrides:    map[string]outbound.OutboundType{"model-c": outbound.OutboundTypeAnthropic},
		},
	}
	if err := ChannelCreate(channel, ctx); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for modelName, routeType := range map[string]outbound.OutboundType{
		"model-a": outbound.OutboundTypeOpenAIResponse,
		"model-b": outbound.OutboundTypeGemini,
		"model-c": outbound.OutboundTypeOpenAIChat,
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ChannelModelRouteLearn(channel.ID, modelName, routeType, ctx); err != nil {
				t.Errorf("ChannelModelRouteLearn(%s): %v", modelName, err)
			}
		}()
	}
	wg.Wait()

	got, err := ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelRoutes.Learned["model-a"] != outbound.OutboundTypeOpenAIResponse || got.ModelRoutes.Learned["model-b"] != outbound.OutboundTypeGemini {
		t.Fatalf("concurrent learned routes were lost: %#v", got.ModelRoutes.Learned)
	}
	if _, ok := got.ModelRoutes.Learned["model-c"]; ok {
		t.Fatalf("manual override was overwritten by learning: %#v", got.ModelRoutes)
	}
}
