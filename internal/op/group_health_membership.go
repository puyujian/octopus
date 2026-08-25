package op

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/bestruirui/octopus/internal/apperror"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

const (
	CodeGroupHealthItemNotFound = "group_health.item_not_found"
	CodeGroupHealthLastItem     = "group_health.last_active_item"
)

func GroupHealthExcludeItem(ctx context.Context, groupID, itemID, attemptID int, allowEmpty bool) (*model.GroupItem, int, error) {
	items, activeCount, err := GroupHealthExcludeItems(ctx, groupID, map[int]int{itemID: attemptID}, allowEmpty)
	if err != nil {
		return nil, activeCount, err
	}
	return &items[0], activeCount, nil
}

// GroupHealthExcludeItems excludes an exact set of group items atomically.
// attemptByItem maps each group-item ID to the failed health-attempt that caused the exclusion.
func GroupHealthExcludeItems(ctx context.Context, groupID int, attemptByItem map[int]int, allowEmpty bool) ([]model.GroupItem, int, error) {
	if len(attemptByItem) == 0 {
		var activeCount int64
		err := db.GetDB().WithContext(ctx).Model(&model.GroupItem{}).Where("group_id = ? AND excluded_at IS NULL", groupID).Count(&activeCount).Error
		return nil, int(activeCount), err
	}
	tx := db.GetDB().WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer tx.Rollback()

	itemIDs := make([]int, 0, len(attemptByItem))
	for itemID := range attemptByItem {
		itemIDs = append(itemIDs, itemID)
	}
	var items []model.GroupItem
	if err := tx.Where("group_id = ? AND id IN ?", groupID, itemIDs).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	if len(items) != len(itemIDs) {
		return nil, 0, apperror.New(CodeGroupHealthItemNotFound, "group item not found").WithStatus(http.StatusNotFound)
	}
	var activeCount int64
	if err := tx.Model(&model.GroupItem{}).Where("group_id = ? AND excluded_at IS NULL", groupID).Count(&activeCount).Error; err != nil {
		return nil, 0, err
	}
	activeTargets := int64(0)
	for i := range items {
		if items[i].ExcludedAt == nil {
			activeTargets++
		}
	}
	if activeCount-activeTargets <= 0 && activeTargets > 0 && !allowEmpty {
		return nil, int(activeCount), apperror.New(CodeGroupHealthLastItem, "excluding these items would leave the group empty").WithStatus(http.StatusConflict)
	}
	now := time.Now()
	for i := range items {
		if items[i].ExcludedAt != nil {
			continue
		}
		attemptID := attemptByItem[items[i].ID]
		if err := tx.Model(&model.GroupItem{}).Where("id = ? AND group_id = ?", items[i].ID, groupID).Updates(map[string]any{"excluded_at": now, "excluded_by_attempt_id": attemptID}).Error; err != nil {
			return nil, 0, err
		}
		items[i].ExcludedAt = &now
		items[i].ExcludedByAttemptID = &attemptID
	}
	activeCount -= activeTargets
	if err := syncActivePresetTx(tx, groupID); err != nil {
		return nil, 0, err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, 0, err
	}
	if err := groupRefreshCacheByID(groupID, ctx); err != nil {
		return nil, 0, fmt.Errorf("refresh group cache: %w", err)
	}
	channelIDs := make([]int, 0, len(items))
	for _, item := range items {
		channelIDs = append(channelIDs, item.ChannelID)
	}
	resetBalancerStateForChannels(channelIDs...)
	return items, int(activeCount), nil
}

func GroupHealthRestoreItem(ctx context.Context, groupID, itemID int) (*model.GroupItem, int, error) {
	items, activeCount, err := GroupHealthRestoreItems(ctx, groupID, []int{itemID})
	if err != nil {
		return nil, activeCount, err
	}
	return &items[0], activeCount, nil
}

// GroupHealthRestoreItems restores an exact set of excluded items atomically.
func GroupHealthRestoreItems(ctx context.Context, groupID int, itemIDs []int) ([]model.GroupItem, int, error) {
	if len(itemIDs) == 0 {
		var activeCount int64
		err := db.GetDB().WithContext(ctx).Model(&model.GroupItem{}).Where("group_id = ? AND excluded_at IS NULL", groupID).Count(&activeCount).Error
		return nil, int(activeCount), err
	}
	tx := db.GetDB().WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer tx.Rollback()

	var items []model.GroupItem
	if err := tx.Where("group_id = ? AND id IN ?", groupID, itemIDs).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	if len(items) != len(itemIDs) {
		return nil, 0, apperror.New(CodeGroupHealthItemNotFound, "group item not found").WithStatus(http.StatusNotFound)
	}
	if err := tx.Model(&model.GroupItem{}).Where("group_id = ? AND id IN ?", groupID, itemIDs).Updates(map[string]any{"excluded_at": nil, "excluded_by_attempt_id": nil}).Error; err != nil {
		return nil, 0, err
	}
	for i := range items {
		items[i].ExcludedAt = nil
		items[i].ExcludedByAttemptID = nil
	}
	if err := syncActivePresetTx(tx, groupID); err != nil {
		return nil, 0, err
	}
	var activeCount int64
	if err := tx.Model(&model.GroupItem{}).Where("group_id = ? AND excluded_at IS NULL", groupID).Count(&activeCount).Error; err != nil {
		return nil, 0, err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, 0, err
	}
	if err := groupRefreshCacheByID(groupID, ctx); err != nil {
		return nil, 0, fmt.Errorf("refresh group cache: %w", err)
	}
	channelIDs := make([]int, 0, len(items))
	for _, item := range items {
		channelIDs = append(channelIDs, item.ChannelID)
	}
	resetBalancerStateForChannels(channelIDs...)
	return items, int(activeCount), nil
}
