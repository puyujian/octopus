package op

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

type GroupHealthRepository struct{}

func NewGroupHealthRepository() *GroupHealthRepository {
	return &GroupHealthRepository{}
}

func (r *GroupHealthRepository) CreateRunningSnapshot(ctx context.Context, group model.Group, probeMode model.GroupHealthProbeMode) (*model.GroupHealthSnapshot, error) {
	snapshot := &model.GroupHealthSnapshot{
		GroupID:      group.ID,
		GroupName:    group.Name,
		GroupMode:    group.Mode,
		ProbeMode:    probeMode,
		RequestModel: group.Name,
		Status:       model.GroupHealthStatusRunning,
		StartedAt:    time.Now(),
	}
	if err := db.GetDB().WithContext(ctx).Create(snapshot).Error; err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (r *GroupHealthRepository) AppendAttempt(ctx context.Context, snapshotID int, attempt model.GroupHealthAttempt) error {
	attempt.SnapshotID = snapshotID
	return db.GetDB().WithContext(ctx).Create(&attempt).Error
}

func (r *GroupHealthRepository) FinishSnapshot(ctx context.Context, snapshotID int, status model.GroupHealthStatus, successfulChannelID *int, durationMS int64, message string, finishedAt time.Time) error {
	update := map[string]any{
		"status":      status,
		"finished_at": finishedAt,
		"duration_ms": durationMS,
		"message":     message,
	}
	if successfulChannelID != nil {
		update["successful_channel_id"] = *successfulChannelID
	} else {
		update["successful_channel_id"] = nil
	}
	return db.GetDB().WithContext(ctx).
		Model(&model.GroupHealthSnapshot{}).
		Where("id = ?", snapshotID).
		Updates(update).Error
}

func (r *GroupHealthRepository) GetLatestSnapshotByGroupID(ctx context.Context, groupID int) (*model.GroupHealthSnapshot, error) {
	var snapshot model.GroupHealthSnapshot
	err := db.GetDB().WithContext(ctx).
		Preload("Attempts", func(tx *gorm.DB) *gorm.DB {
			return tx.Order("priority ASC, id ASC")
		}).
		Where("group_id = ?", groupID).
		Order("id DESC").
		First(&snapshot).Error
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (r *GroupHealthRepository) GetAttemptByID(ctx context.Context, attemptID int) (*model.GroupHealthAttempt, error) {
	var attempt model.GroupHealthAttempt
	if err := db.GetDB().WithContext(ctx).First(&attempt, attemptID).Error; err != nil {
		return nil, err
	}
	return &attempt, nil
}

func (r *GroupHealthRepository) GetSnapshotByID(ctx context.Context, snapshotID int) (*model.GroupHealthSnapshot, error) {
	var snapshot model.GroupHealthSnapshot
	if err := db.GetDB().WithContext(ctx).First(&snapshot, snapshotID).Error; err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (r *GroupHealthRepository) ListLatestSnapshots(ctx context.Context) ([]model.GroupHealthSnapshot, error) {
	var snapshots []model.GroupHealthSnapshot
	err := db.GetDB().WithContext(ctx).
		Preload("Attempts", func(tx *gorm.DB) *gorm.DB {
			return tx.Order("priority ASC, id ASC")
		}).
		Where("id IN (?)",
			db.GetDB().Model(&model.GroupHealthSnapshot{}).
				Select("MAX(id)").
				Group("group_id"),
		).
		Order("group_name ASC").
		Find(&snapshots).Error
	return snapshots, err
}

func (r *GroupHealthRepository) GetRunningSnapshotByGroupID(ctx context.Context, groupID int) (*model.GroupHealthSnapshot, error) {
	var snapshot model.GroupHealthSnapshot
	err := db.GetDB().WithContext(ctx).
		Where("group_id = ? AND status = ?", groupID, model.GroupHealthStatusRunning).
		Order("id DESC").
		First(&snapshot).Error
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (r *GroupHealthRepository) ListGroupHealthViews(ctx context.Context) ([]model.GroupHealthGroupView, error) {
	var groups []model.Group
	if err := db.GetDB().WithContext(ctx).Preload("Items").Find(&groups).Error; err != nil {
		return nil, err
	}
	var err error
	snapshots, err := r.ListLatestSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	latestByGroupID := make(map[int]model.GroupHealthSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		latestByGroupID[snapshot.GroupID] = snapshot
	}
	views := make([]model.GroupHealthGroupView, 0, len(groups))
	for _, group := range groups {
		view := model.GroupHealthGroupView{
			GroupID:   group.ID,
			GroupName: group.Name,
			GroupMode: group.Mode,
		}
		if snapshot, ok := latestByGroupID[group.ID]; ok {
			copySnapshot := snapshot
			view.Latest = &copySnapshot
		}
		if err := r.enrichView(ctx, &view, group.Items); err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].GroupName != views[j].GroupName {
			return views[i].GroupName < views[j].GroupName
		}
		return views[i].GroupID < views[j].GroupID
	})
	return views, nil
}

func (r *GroupHealthRepository) GetGroupHealthViewByID(ctx context.Context, groupID int) (*model.GroupHealthGroupView, error) {
	var group model.Group
	if err := db.GetDB().WithContext(ctx).Preload("Items").First(&group, groupID).Error; err != nil {
		return nil, err
	}
	view := &model.GroupHealthGroupView{
		GroupID:   group.ID,
		GroupName: group.Name,
		GroupMode: group.Mode,
	}
	snapshot, err := r.GetLatestSnapshotByGroupID(ctx, groupID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if enrichErr := r.enrichView(ctx, view, group.Items); enrichErr != nil {
				return nil, enrichErr
			}
			return view, nil
		}
		return nil, err
	}
	view.Latest = snapshot
	if err := r.enrichView(ctx, view, group.Items); err != nil {
		return nil, err
	}
	return view, nil
}

func (r *GroupHealthRepository) enrichView(ctx context.Context, view *model.GroupHealthGroupView, items []model.GroupItem) error {
	itemByID := make(map[int]model.GroupItem, len(items))
	for _, item := range items {
		itemByID[item.ID] = item
		if item.ExcludedAt == nil {
			view.ActiveItemCount++
		}
	}
	if view.Latest != nil {
		for i := range view.Latest.Attempts {
			item, ok := itemByID[view.Latest.Attempts[i].GroupItemID]
			switch {
			case !ok:
				view.Latest.Attempts[i].MembershipState = model.GroupHealthMembershipStateMissing
			case item.ExcludedAt != nil:
				view.Latest.Attempts[i].MembershipState = model.GroupHealthMembershipStateExcluded
			default:
				view.Latest.Attempts[i].MembershipState = model.GroupHealthMembershipStateActive
			}
		}
	}

	for _, item := range items {
		if item.ExcludedAt == nil {
			continue
		}
		var channel model.Channel
		channelName := fmt.Sprintf("channel-%d", item.ChannelID)
		channelEnabled := false
		if err := db.GetDB().WithContext(ctx).First(&channel, item.ChannelID).Error; err == nil {
			channelName, channelEnabled = channel.Name, channel.Enabled
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		excluded := model.GroupHealthExcludedItem{
			ID: item.ID, GroupID: item.GroupID, ChannelID: item.ChannelID,
			ChannelName: channelName, ChannelEnabled: channelEnabled, ModelName: item.ModelName,
			Priority: item.Priority, Weight: item.Weight, ExcludedAt: item.ExcludedAt,
			ExcludedByAttemptID: item.ExcludedByAttemptID,
		}
		var attempt model.GroupHealthAttempt
		query := db.GetDB().WithContext(ctx).Where("group_item_id = ? AND status = ?", item.ID, model.GroupHealthAttemptStatusFailed)
		if item.ExcludedByAttemptID != nil {
			query = query.Where("id = ?", *item.ExcludedByAttemptID)
		}
		if err := query.Order("id DESC").First(&attempt).Error; err == nil {
			excluded.HTTPStatus = attempt.HTTPStatus
			excluded.DurationMS = attempt.DurationMS
			excluded.ErrorMessage = attempt.ErrorMessage
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		view.ExcludedItems = append(view.ExcludedItems, excluded)
	}
	sort.Slice(view.ExcludedItems, func(i, j int) bool { return view.ExcludedItems[i].ID < view.ExcludedItems[j].ID })
	return nil
}
