package task

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bestruirui/octopus/internal/helper"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/utils/diff"
	"github.com/bestruirui/octopus/internal/utils/xstrings"
)

var (
	ErrChannelModelSyncManaged    = errors.New("managed site channel cannot be synced here")
	ErrChannelModelSyncInProgress = errors.New("channel model sync is already in progress")
	activeChannelModelSyncs       sync.Map
)

type ChannelModelSyncResult struct {
	ChannelID     int      `json:"channel_id"`
	ModelCount    int      `json:"model_count"`
	AddedModels   []string `json:"added_models"`
	RemovedModels []string `json:"removed_models"`
	Models        []string `json:"-"`
}

func SyncChannelModels(ctx context.Context, channelID int) (*ChannelModelSyncResult, error) {
	if channelID <= 0 {
		return nil, fmt.Errorf("invalid channel id")
	}
	if _, loaded := activeChannelModelSyncs.LoadOrStore(channelID, struct{}{}); loaded {
		return nil, ErrChannelModelSyncInProgress
	}
	defer activeChannelModelSyncs.Delete(channelID)

	channel, err := op.ChannelGet(channelID, ctx)
	if err != nil {
		return nil, err
	}
	if _, managed, err := op.ChannelManagedBinding(channelID, ctx); err != nil {
		return nil, err
	} else if managed {
		return nil, ErrChannelModelSyncManaged
	}

	fetchedModels, err := helper.FetchModels(ctx, *channel)
	if err != nil {
		return nil, err
	}
	return applyFetchedChannelModels(ctx, *channel, fetchedModels)
}

func applyFetchedChannelModels(ctx context.Context, channel model.Channel, fetchedModels []string) (*ChannelModelSyncResult, error) {
	oldModels := uniqueModelNames(xstrings.SplitTrimCompact(",", channel.Model))
	newModels := uniqueModelNames(xstrings.TrimCompact(fetchedModels))
	removedModels, addedModels := diff.Diff(oldModels, newModels)
	if addedModels == nil {
		addedModels = []string{}
	}
	if removedModels == nil {
		removedModels = []string{}
	}

	updatedChannel := &channel
	if len(removedModels) > 0 || len(addedModels) > 0 {
		modelValue := strings.Join(newModels, ",")
		var err error
		updatedChannel, err = op.ChannelUpdate(&model.ChannelUpdateRequest{ID: channel.ID, Model: &modelValue}, ctx)
		if err != nil {
			return nil, err
		}
	}

	customModels := make(map[string]struct{})
	for _, modelName := range xstrings.SplitTrimCompact(",", channel.CustomModel) {
		customModels[strings.ToLower(modelName)] = struct{}{}
	}
	removedGroupItems := make([]model.GroupIDAndLLMName, 0, len(removedModels))
	for _, modelName := range removedModels {
		if _, preserved := customModels[strings.ToLower(modelName)]; preserved {
			continue
		}
		removedGroupItems = append(removedGroupItems, model.GroupIDAndLLMName{ChannelID: channel.ID, ModelName: modelName})
	}
	if err := op.GroupItemBatchDelByChannelAndModels(removedGroupItems, ctx); err != nil {
		return nil, err
	}
	if err := helper.LLMPriceAddToDB(addedModels, ctx); err != nil {
		return nil, err
	}
	if len(newModels) > 0 {
		helper.ChannelAutoGroup(updatedChannel, ctx)
	}

	return &ChannelModelSyncResult{
		ChannelID:     channel.ID,
		ModelCount:    len(newModels),
		AddedModels:   addedModels,
		RemovedModels: removedModels,
		Models:        newModels,
	}, nil
}

func uniqueModelNames(models []string) []string {
	result := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, modelName := range models {
		modelName = strings.TrimSpace(modelName)
		key := strings.ToLower(modelName)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, modelName)
	}
	return result
}
