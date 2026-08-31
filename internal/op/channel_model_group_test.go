package op

import (
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestChannelModelGroupPreviewOrdersMatchesAndReportsMembership(t *testing.T) {
	ctx := setupSiteOpTestDB(t)
	if err := InitCache(); err != nil {
		t.Fatal(err)
	}
	channel := &model.Channel{Name: "preview-channel", Type: outbound.OutboundTypeOpenAIChat, Enabled: true, Model: "gpt-4o-mini"}
	if err := ChannelCreate(channel, ctx); err != nil {
		t.Fatal(err)
	}
	exact := &model.Group{Name: "gpt-4o-mini", Mode: model.GroupModeRoundRobin}
	regex := &model.Group{Name: "OpenAI", MatchRegex: `^gpt-4`, Mode: model.GroupModeRoundRobin}
	fuzzy := &model.Group{Name: "4o", Mode: model.GroupModeRoundRobin}
	for _, group := range []*model.Group{exact, regex, fuzzy} {
		if err := GroupCreate(group, ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.GetDB().WithContext(ctx).Create(&model.GroupItem{GroupID: exact.ID, ChannelID: channel.ID, ModelName: "gpt-4o-mini"}).Error; err != nil {
		t.Fatal(err)
	}
	excludedAt := time.Now()
	if err := db.GetDB().WithContext(ctx).Create(&model.GroupItem{GroupID: regex.ID, ChannelID: channel.ID, ModelName: "gpt-4o-mini", ExcludedAt: &excludedAt}).Error; err != nil {
		t.Fatal(err)
	}

	preview, err := ChannelModelGroupPreview(ctx, []model.ChannelModelHealthTarget{{ChannelID: channel.ID, ModelName: "gpt-4o-mini"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != 1 || len(preview[0].Candidates) != 3 {
		t.Fatalf("unexpected preview: %#v", preview)
	}
	gotReasons := []string{preview[0].Candidates[0].Reason, preview[0].Candidates[1].Reason, preview[0].Candidates[2].Reason}
	if gotReasons[0] != "exact" || gotReasons[1] != "regex" || gotReasons[2] != "fuzzy" {
		t.Fatalf("unexpected candidate precedence: %#v", gotReasons)
	}
	if len(preview[0].ExistingGroupIDs) != 1 || preview[0].ExistingGroupIDs[0] != exact.ID || len(preview[0].ExcludedGroupIDs) != 1 || preview[0].ExcludedGroupIDs[0] != regex.ID {
		t.Fatalf("membership was not classified: %#v", preview[0])
	}
}

func TestChannelModelGroupApplyHandlesDuplicateExcludedAndExplicitCreation(t *testing.T) {
	ctx := setupSiteOpTestDB(t)
	if err := InitCache(); err != nil {
		t.Fatal(err)
	}
	channel := &model.Channel{Name: "apply-channel", Type: outbound.OutboundTypeOpenAIChat, Enabled: true, Model: "model-a,model-b"}
	if err := ChannelCreate(channel, ctx); err != nil {
		t.Fatal(err)
	}
	active := &model.Group{Name: "active", Mode: model.GroupModeRoundRobin}
	excluded := &model.Group{Name: "excluded", Mode: model.GroupModeRoundRobin}
	for _, group := range []*model.Group{active, excluded} {
		if err := GroupCreate(group, ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.GetDB().WithContext(ctx).Create(&model.GroupItem{GroupID: active.ID, ChannelID: channel.ID, ModelName: "model-a"}).Error; err != nil {
		t.Fatal(err)
	}
	excludedAt := time.Now()
	if err := db.GetDB().WithContext(ctx).Create(&model.GroupItem{GroupID: excluded.ID, ChannelID: channel.ID, ModelName: "model-a", ExcludedAt: &excludedAt}).Error; err != nil {
		t.Fatal(err)
	}

	oldReset := resetRelayBalancerStateForChannel
	resetIDs := []int{}
	RegisterRelayBalancerStateReset(func(id int) { resetIDs = append(resetIDs, id) })
	t.Cleanup(func() { resetRelayBalancerStateForChannel = oldReset })
	result, err := ChannelModelGroupApply(ctx, model.ChannelModelGroupApplyRequest{Items: []model.ChannelModelGroupApplyItem{
		{ChannelID: channel.ID, ModelName: "model-a", GroupID: &active.ID},
		{ChannelID: channel.ID, ModelName: "model-a", GroupID: &excluded.ID},
		{ChannelID: channel.ID, ModelName: "model-a", CreateGroupName: "model-a"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Existing != 1 || result.Excluded != 1 || result.Added != 1 || result.CreatedGroups != 1 {
		t.Fatalf("unexpected apply result: %#v", result)
	}
	var excludedItem model.GroupItem
	if err := db.GetDB().WithContext(ctx).Where("group_id = ? AND channel_id = ? AND model_name = ?", excluded.ID, channel.ID, "model-a").First(&excludedItem).Error; err != nil {
		t.Fatal(err)
	}
	if excludedItem.ExcludedAt == nil {
		t.Fatal("excluded member was silently restored")
	}
	if len(resetIDs) != 1 || resetIDs[0] != channel.ID {
		t.Fatalf("balancer state was not reset once: %#v", resetIDs)
	}

	missingGroupID := 999999
	if _, err := ChannelModelGroupApply(ctx, model.ChannelModelGroupApplyRequest{Items: []model.ChannelModelGroupApplyItem{
		{ChannelID: channel.ID, ModelName: "model-b", GroupID: &active.ID},
		{ChannelID: channel.ID, ModelName: "model-b", GroupID: &missingGroupID},
	}}); err == nil {
		t.Fatal("expected missing group validation error")
	}
	var count int64
	if err := db.GetDB().WithContext(ctx).Model(&model.GroupItem{}).Where("channel_id = ? AND model_name = ?", channel.ID, "model-b").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("validation failure partially committed %d items", count)
	}
}
