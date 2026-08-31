package op

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/apperror"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/dlclark/regexp2"
	"gorm.io/gorm"
)

func ChannelModelGroupPreview(ctx context.Context, targets []model.ChannelModelHealthTarget) ([]model.ChannelModelGroupPreviewItem, error) {
	var groups []model.Group
	if err := db.GetDB().WithContext(ctx).Preload("Items").Order("LOWER(name) ASC, id ASC").Find(&groups).Error; err != nil {
		return nil, err
	}
	result := make([]model.ChannelModelGroupPreviewItem, 0, len(targets))
	for _, target := range targets {
		item := model.ChannelModelGroupPreviewItem{
			ChannelID:        target.ChannelID,
			ModelName:        target.ModelName,
			ExistingGroupIDs: []int{},
			ExcludedGroupIDs: []int{},
			Candidates:       []model.ChannelModelGroupCandidate{},
		}
		for _, group := range groups {
			for _, groupItem := range group.Items {
				if groupItem.ChannelID == target.ChannelID && groupItem.ModelName == target.ModelName {
					if groupItem.ExcludedAt == nil {
						item.ExistingGroupIDs = append(item.ExistingGroupIDs, group.ID)
					} else {
						item.ExcludedGroupIDs = append(item.ExcludedGroupIDs, group.ID)
					}
					break
				}
			}
			groupName := strings.ToLower(strings.TrimSpace(group.Name))
			modelName := strings.ToLower(strings.TrimSpace(target.ModelName))
			if groupName != "" && groupName == modelName {
				item.Candidates = append(item.Candidates, model.ChannelModelGroupCandidate{GroupID: group.ID, GroupName: group.Name, Reason: "exact"})
				continue
			}
			if group.MatchRegex != "" {
				re, compileErr := regexp2.Compile(group.MatchRegex, regexp2.ECMAScript)
				if compileErr == nil {
					re.MatchTimeout = 200 * time.Millisecond
					if matched, matchErr := re.MatchString(target.ModelName); matchErr == nil && matched {
						item.Candidates = append(item.Candidates, model.ChannelModelGroupCandidate{GroupID: group.ID, GroupName: group.Name, Reason: "regex"})
						continue
					}
				}
			}
			if groupName != "" && strings.Contains(modelName, groupName) {
				item.Candidates = append(item.Candidates, model.ChannelModelGroupCandidate{GroupID: group.ID, GroupName: group.Name, Reason: "fuzzy"})
			}
		}
		sort.SliceStable(item.Candidates, func(left, right int) bool {
			rank := map[string]int{"exact": 0, "regex": 1, "fuzzy": 2}
			if rank[item.Candidates[left].Reason] != rank[item.Candidates[right].Reason] {
				return rank[item.Candidates[left].Reason] < rank[item.Candidates[right].Reason]
			}
			if item.Candidates[left].GroupName != item.Candidates[right].GroupName {
				return strings.ToLower(item.Candidates[left].GroupName) < strings.ToLower(item.Candidates[right].GroupName)
			}
			return item.Candidates[left].GroupID < item.Candidates[right].GroupID
		})
		result = append(result, item)
	}
	return result, nil
}

func ChannelModelGroupApply(ctx context.Context, req model.ChannelModelGroupApplyRequest) (*model.ChannelModelGroupApplyResult, error) {
	if len(req.Items) == 0 {
		return nil, channelModelGroupValidationError("no group items selected")
	}
	result := &model.ChannelModelGroupApplyResult{Failed: []string{}}
	tx := db.GetDB().WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	defer tx.Rollback()

	for _, item := range req.Items {
		modelName := strings.TrimSpace(item.ModelName)
		if item.ChannelID <= 0 || modelName == "" {
			return nil, channelModelGroupValidationError("invalid channel model target")
		}
		var channel model.Channel
		if err := tx.First(&channel, item.ChannelID).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return nil, channelModelGroupValidationError(fmt.Sprintf("channel %d not found", item.ChannelID))
			}
			return nil, err
		}
		if !channelContainsModel(channel, modelName) {
			return nil, channelModelGroupValidationError(fmt.Sprintf("model %s is not configured on channel %d", modelName, item.ChannelID))
		}
		if item.GroupID != nil {
			if *item.GroupID <= 0 {
				return nil, channelModelGroupValidationError(fmt.Sprintf("invalid target group for model %s", modelName))
			}
			if strings.TrimSpace(item.CreateGroupName) != "" {
				return nil, channelModelGroupValidationError(fmt.Sprintf("model %s has multiple target groups", modelName))
			}
			var count int64
			if err := tx.Model(&model.Group{}).Where("id = ?", *item.GroupID).Count(&count).Error; err != nil {
				return nil, err
			}
			if count == 0 {
				return nil, channelModelGroupValidationError(fmt.Sprintf("target group %d not found", *item.GroupID))
			}
		} else if strings.TrimSpace(item.CreateGroupName) == "" {
			return nil, channelModelGroupValidationError(fmt.Sprintf("%s: no target group", modelName))
		}
	}

	created := map[string]int{}
	affectedGroupIDs := map[int]struct{}{}
	affectedChannelIDs := map[int]struct{}{}
	for _, item := range req.Items {
		if item.ChannelID <= 0 || strings.TrimSpace(item.ModelName) == "" {
			result.Failed = append(result.Failed, "invalid target")
			continue
		}
		groupID := 0
		if item.GroupID != nil {
			groupID = *item.GroupID
		}
		if groupID == 0 && strings.TrimSpace(item.CreateGroupName) != "" {
			name := strings.TrimSpace(item.CreateGroupName)
			if id, ok := created[strings.ToLower(name)]; ok {
				groupID = id
			} else {
				var existingGroup model.Group
				lookupErr := tx.Where("LOWER(name) = LOWER(?)", name).First(&existingGroup).Error
				if lookupErr == nil {
					groupID = existingGroup.ID
				} else if lookupErr == gorm.ErrRecordNotFound {
					group := model.Group{Name: name, Mode: model.GroupModeRoundRobin, MaxRetries: 3}
					if err := tx.Create(&group).Error; err != nil {
						tx.Rollback()
						return nil, err
					}
					created[strings.ToLower(name)] = group.ID
					groupID = group.ID
					result.CreatedGroups++
				} else {
					tx.Rollback()
					return nil, lookupErr
				}
			}
		}
		if groupID == 0 {
			result.Failed = append(result.Failed, fmt.Sprintf("%s: no target group", item.ModelName))
			continue
		}

		var existing model.GroupItem
		err := tx.Where("group_id = ? AND channel_id = ? AND model_name = ?", groupID, item.ChannelID, item.ModelName).First(&existing).Error
		if err == nil {
			if existing.ExcludedAt != nil {
				result.Excluded++
			} else {
				result.Existing++
			}
			continue
		}
		if err != nil && err != gorm.ErrRecordNotFound {
			tx.Rollback()
			return nil, err
		}

		var maxPriority int
		if err := tx.Model(&model.GroupItem{}).Where("group_id = ?", groupID).Select("COALESCE(MAX(priority),0)").Scan(&maxPriority).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
		groupItem := model.GroupItem{GroupID: groupID, ChannelID: item.ChannelID, ModelName: item.ModelName, Priority: maxPriority + 1, Weight: 1}
		if err := tx.Create(&groupItem).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
		affectedGroupIDs[groupID] = struct{}{}
		affectedChannelIDs[item.ChannelID] = struct{}{}
		result.Added++
	}
	for groupID := range affectedGroupIDs {
		if err := syncActivePresetTx(tx, groupID); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	for _, groupID := range created {
		affectedGroupIDs[groupID] = struct{}{}
	}
	ids := make([]int, 0, len(affectedGroupIDs))
	for groupID := range affectedGroupIDs {
		ids = append(ids, groupID)
	}
	if err := groupRefreshCacheByIDs(ids, ctx); err != nil {
		return nil, err
	}
	channelIDs := make([]int, 0, len(affectedChannelIDs))
	for channelID := range affectedChannelIDs {
		channelIDs = append(channelIDs, channelID)
	}
	resetBalancerStateForChannels(channelIDs...)
	return result, nil
}

func channelContainsModel(channel model.Channel, name string) bool {
	for _, raw := range []string{channel.Model, channel.CustomModel} {
		for _, configured := range strings.Split(raw, ",") {
			if strings.TrimSpace(configured) == name {
				return true
			}
		}
	}
	return false
}

func channelModelGroupValidationError(message string) error {
	return apperror.New(apperror.CodeCommonValidationFailed, message).WithStatus(http.StatusBadRequest)
}
