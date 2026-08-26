package op

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/bestruirui/octopus/internal/apperror"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
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
	items, activeCount, err := restoreGroupHealthItems(ctx, groupID, []int{itemID}, nil)
	if err != nil {
		return nil, activeCount, err
	}
	return &items[0], activeCount, nil
}

// GroupHealthRestoreItemAfterProbe atomically restores an excluded item and
// replaces the failed health attempt which caused the exclusion with the newer
// successful probe result. This keeps the latest health view from presenting a
// successfully recovered item as failed and immediately offering to exclude it
// again based on stale evidence.
func GroupHealthRestoreItemAfterProbe(ctx context.Context, groupID, itemID int, probe model.GroupHealthRecoveryProbe) (*model.GroupItem, int, error) {
	items, activeCount, err := restoreGroupHealthItems(ctx, groupID, []int{itemID}, map[int]model.GroupHealthRecoveryProbe{itemID: probe})
	if err != nil {
		return nil, activeCount, err
	}
	return &items[0], activeCount, nil
}

// GroupHealthRestoreItems restores an exact set of excluded items atomically.
func GroupHealthRestoreItems(ctx context.Context, groupID int, itemIDs []int) ([]model.GroupItem, int, error) {
	return restoreGroupHealthItems(ctx, groupID, itemIDs, nil)
}

// GroupHealthRestoreItemsAfterProbe is the batch form of
// GroupHealthRestoreItemAfterProbe. Only items present in probeByItem have their
// originating failed attempt replaced; callers must only include successful
// probes in that map.
func GroupHealthRestoreItemsAfterProbe(ctx context.Context, groupID int, itemIDs []int, probeByItem map[int]model.GroupHealthRecoveryProbe) ([]model.GroupItem, int, error) {
	return restoreGroupHealthItems(ctx, groupID, itemIDs, probeByItem)
}

func restoreGroupHealthItems(ctx context.Context, groupID int, itemIDs []int, probeByItem map[int]model.GroupHealthRecoveryProbe) ([]model.GroupItem, int, error) {
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
	affectedSnapshotIDs := make(map[int]struct{})
	for i := range items {
		probe, hasProbe := probeByItem[items[i].ID]
		if !hasProbe {
			continue
		}
		if !probe.Success {
			return nil, 0, fmt.Errorf("cannot restore group item %d with an unsuccessful probe", items[i].ID)
		}
		if items[i].ExcludedByAttemptID == nil {
			continue
		}
		var attempt model.GroupHealthAttempt
		if err := tx.Where("id = ? AND group_item_id = ?", *items[i].ExcludedByAttemptID, items[i].ID).First(&attempt).Error; err != nil {
			return nil, 0, fmt.Errorf("load exclusion attempt for group item %d: %w", items[i].ID, err)
		}
		if err := tx.Model(&model.GroupHealthAttempt{}).Where("id = ?", attempt.ID).Updates(map[string]any{
			"status":        model.GroupHealthAttemptStatusSuccess,
			"http_status":   probe.HTTPStatus,
			"duration_ms":   probe.DurationMS,
			"error_message": "",
		}).Error; err != nil {
			return nil, 0, fmt.Errorf("record successful recovery for group item %d: %w", items[i].ID, err)
		}
		affectedSnapshotIDs[attempt.SnapshotID] = struct{}{}
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
	for snapshotID := range affectedSnapshotIDs {
		if err := refreshRecoveredSnapshotTx(tx, snapshotID); err != nil {
			return nil, 0, err
		}
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

func refreshRecoveredSnapshotTx(tx *gorm.DB, snapshotID int) error {
	var attempts []model.GroupHealthAttempt
	if err := tx.Where("snapshot_id = ?", snapshotID).Order("priority ASC, id ASC").Find(&attempts).Error; err != nil {
		return fmt.Errorf("load recovered snapshot %d attempts: %w", snapshotID, err)
	}
	successCount := 0
	failedCount := 0
	for i := range attempts {
		switch attempts[i].Status {
		case model.GroupHealthAttemptStatusSuccess:
			successCount++
		case model.GroupHealthAttemptStatusFailed:
			failedCount++
		}
	}
	status := model.GroupHealthStatusFailed
	switch {
	case successCount > 0 && failedCount == 0:
		status = model.GroupHealthStatusSuccess
	case successCount > 0:
		status = model.GroupHealthStatusPartial
	}

	var successfulChannelID any
	sort.SliceStable(attempts, func(i, j int) bool {
		if attempts[i].Priority != attempts[j].Priority {
			return attempts[i].Priority < attempts[j].Priority
		}
		return attempts[i].ID < attempts[j].ID
	})
	for i := range attempts {
		if attempts[i].Status == model.GroupHealthAttemptStatusSuccess {
			successfulChannelID = attempts[i].ChannelID
			break
		}
	}
	probedCount := successCount + failedCount
	message := fmt.Sprintf("%d/%d candidates currently healthy after recovery", successCount, probedCount)
	if err := tx.Model(&model.GroupHealthSnapshot{}).Where("id = ?", snapshotID).Updates(map[string]any{
		"status":                status,
		"successful_channel_id": successfulChannelID,
		"message":               message,
	}).Error; err != nil {
		return fmt.Errorf("refresh recovered snapshot %d: %w", snapshotID, err)
	}
	return nil
}
