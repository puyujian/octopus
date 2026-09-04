package task

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestSyncChannelModelsUpdatesOnlyFetchedModelsAndPreservesCustomMembership(t *testing.T) {
	if db.GetDB() != nil {
		_ = db.Close()
	}
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "sync-test.db"), false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"new-model"},{"id":"new-model"}]}`))
	}))
	defer server.Close()

	ctx := context.Background()
	channel := &model.Channel{
		Name:        "manual-sync",
		Type:        outbound.OutboundTypeOpenAIChat,
		Enabled:     true,
		BaseUrls:    []model.BaseUrl{{URL: server.URL}},
		Keys:        []model.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
		Model:       "old-model,custom-keep",
		CustomModel: "custom-keep",
		AutoGroup:   model.AutoGroupTypeExact,
	}
	if err := op.ChannelCreate(channel, ctx); err != nil {
		t.Fatal(err)
	}
	oldGroup := &model.Group{Name: "old-model", Mode: model.GroupModeRoundRobin}
	customGroup := &model.Group{Name: "custom-keep", Mode: model.GroupModeRoundRobin}
	newGroup := &model.Group{Name: "new-model", Mode: model.GroupModeRoundRobin}
	for _, group := range []*model.Group{oldGroup, customGroup, newGroup} {
		if err := op.GroupCreate(group, ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []*model.GroupItem{
		{GroupID: oldGroup.ID, ChannelID: channel.ID, ModelName: "old-model", Weight: 1},
		{GroupID: customGroup.ID, ChannelID: channel.ID, ModelName: "custom-keep", Weight: 1},
	} {
		if err := op.GroupItemAdd(item, ctx); err != nil {
			t.Fatal(err)
		}
	}

	result, err := SyncChannelModels(ctx, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelCount != 1 || len(result.AddedModels) != 1 || result.AddedModels[0] != "new-model" || len(result.RemovedModels) != 2 {
		t.Fatalf("unexpected sync result: %#v", result)
	}
	updated, err := op.ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Model != "new-model" || updated.CustomModel != "custom-keep" {
		t.Fatalf("unexpected channel models: model=%q custom=%q", updated.Model, updated.CustomModel)
	}

	assertGroupModelCount := func(groupID int, modelName string, want int64) {
		t.Helper()
		var count int64
		if err := db.GetDB().WithContext(ctx).Model(&model.GroupItem{}).
			Where("group_id = ? AND channel_id = ? AND model_name = ?", groupID, channel.ID, modelName).
			Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("group %d model %s count=%d want=%d", groupID, modelName, count, want)
		}
	}
	assertGroupModelCount(oldGroup.ID, "old-model", 0)
	assertGroupModelCount(customGroup.ID, "custom-keep", 1)
	assertGroupModelCount(newGroup.ID, "new-model", 1)
	if _, err := op.LLMGet("new-model"); err != nil {
		t.Fatalf("new model price row was not created: %v", err)
	}

	unchanged, err := SyncChannelModels(ctx, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.AddedModels == nil || unchanged.RemovedModels == nil {
		t.Fatalf("unchanged sync must return empty arrays, got %#v", unchanged)
	}
	if len(unchanged.AddedModels) != 0 || len(unchanged.RemovedModels) != 0 {
		t.Fatalf("unchanged sync reported changes: %#v", unchanged)
	}
}
