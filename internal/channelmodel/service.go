package channelmodel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/apperror"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/grouphealth"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/utils/safe"
	"gorm.io/gorm/clause"
)

const healthTTL = 10 * time.Minute

var activeTargets sync.Map
var workerSlots = make(chan struct{}, 4)

func targetKey(target model.ChannelModelHealthTarget) string {
	return fmt.Sprintf("%d|%s", target.ChannelID, target.ModelName)
}

func fingerprint(channel model.Channel) string {
	type config struct {
		Type, URLs, Proxy, ProxyID, Headers, Param, Models any
		Keys                                               []string
	}
	keys := make([]string, 0, len(channel.Keys))
	for _, key := range channel.Keys {
		if !key.Enabled || strings.TrimSpace(key.ChannelKey) == "" {
			continue
		}
		digest := sha256.Sum256([]byte(key.ChannelKey))
		keys = append(keys, fmt.Sprintf("%d:%s", key.ID, hex.EncodeToString(digest[:])))
	}
	sort.Strings(keys)
	encoded, _ := json.Marshal(config{channel.Type, channel.BaseUrls, channel.ProxyMode, channel.ProxyConfigID, channel.CustomHeader, channel.ParamOverride, channel.Model + "\x00" + channel.CustomModel, keys})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func Query(ctx context.Context, targets []model.ChannelModelHealthTarget) ([]model.ChannelModelHealth, error) {
	if len(targets) == 0 {
		return []model.ChannelModelHealth{}, nil
	}
	conditions := make([][]any, 0, len(targets))
	for _, target := range targets {
		conditions = append(conditions, []any{target.ChannelID, target.ModelName})
	}
	var rows []model.ChannelModelHealth
	if err := db.GetDB().WithContext(ctx).Where("(channel_id, model_name) IN ?", conditions).Find(&rows).Error; err != nil {
		return nil, err
	}
	now := time.Now()
	for index := range rows {
		key := targetKey(model.ChannelModelHealthTarget{ChannelID: rows[index].ChannelID, ModelName: rows[index].ModelName})
		if (rows[index].Status == model.ChannelModelHealthQueued || rows[index].Status == model.ChannelModelHealthRunning) && !isActive(key) {
			rows[index].Status = model.ChannelModelHealthInterrupted
			rows[index].ErrorMessage = "probe interrupted by service restart"
			rows[index].CheckedAt = &now
			if err := db.GetDB().WithContext(ctx).Model(&model.ChannelModelHealth{}).
				Where("id = ? AND status IN ?", rows[index].ID, []model.ChannelModelHealthStatus{model.ChannelModelHealthQueued, model.ChannelModelHealthRunning}).
				Updates(map[string]any{"status": rows[index].Status, "error_message": rows[index].ErrorMessage, "checked_at": now}).Error; err != nil {
				return nil, err
			}
		}
		channel, err := op.ChannelGet(rows[index].ChannelID, ctx)
		if err != nil {
			continue
		}
		finished := rows[index].Status == model.ChannelModelHealthSuccess || rows[index].Status == model.ChannelModelHealthFailed
		if finished && (rows[index].CheckedAt == nil || now.Sub(*rows[index].CheckedAt) > healthTTL || rows[index].ConfigFingerprint != fingerprint(*channel)) {
			rows[index].Status = model.ChannelModelHealthStale
		}
	}
	return rows, nil
}

func isActive(key string) bool {
	_, active := activeTargets.Load(key)
	return active
}

func Run(ctx context.Context, targets []model.ChannelModelHealthTarget) (string, int, error) {
	unique := make(map[string]model.ChannelModelHealthTarget)
	for _, target := range targets {
		target.ModelName = strings.TrimSpace(target.ModelName)
		if target.ChannelID <= 0 || target.ModelName == "" {
			continue
		}
		key := targetKey(target)
		if _, running := activeTargets.LoadOrStore(key, struct{}{}); running {
			continue
		}
		unique[key] = target
	}
	if len(unique) == 0 {
		return "", 0, fmt.Errorf("no available health targets")
	}
	taskID := fmt.Sprintf("%d", time.Now().UnixNano())
	for _, target := range unique {
		channel, _ := op.ChannelGet(target.ChannelID, ctx)
		configFingerprint := ""
		if channel != nil {
			configFingerprint = fingerprint(*channel)
		}
		if err := save(ctx, target, model.ChannelModelHealthQueued, grouphealth.ProbeResult{}, model.ChannelKey{}, configFingerprint, nil); err != nil {
			for reservedKey, reservedTarget := range unique {
				activeTargets.Delete(reservedKey)
				_ = saveFailure(context.Background(), reservedTarget, "failed to queue probe: "+err.Error(), "")
			}
			return "", 0, err
		}
	}
	safe.Go("channel-model-health", func() {
		var wait sync.WaitGroup
		for key, target := range unique {
			key, target := key, target
			wait.Add(1)
			go func() {
				defer wait.Done()
				defer activeTargets.Delete(key)
				workerSlots <- struct{}{}
				defer func() { <-workerSlots }()
				_ = runTarget(context.Background(), target)
			}()
		}
		wait.Wait()
	})
	return taskID, len(unique), nil
}

func runTarget(ctx context.Context, target model.ChannelModelHealthTarget) error {
	channel, err := op.ChannelGet(target.ChannelID, ctx)
	if err != nil {
		return saveFailure(ctx, target, err.Error(), "")
	}
	configFingerprint := fingerprint(*channel)
	if !containsModel(*channel, target.ModelName) {
		return saveFailure(ctx, target, "model is not configured on channel", configFingerprint)
	}
	if err := save(ctx, target, model.ChannelModelHealthRunning, grouphealth.ProbeResult{}, model.ChannelKey{}, configFingerprint, nil); err != nil {
		return err
	}
	keys := enabledKeys(channel.Keys)
	if len(keys) == 0 {
		return saveFailure(ctx, target, "no available key", configFingerprint)
	}
	var result grouphealth.ProbeResult
	var usedKey model.ChannelKey
	var totalDuration int64
	for index, key := range keys {
		if index >= 3 {
			break
		}
		usedKey = key
		result = grouphealth.NewProber().RunCandidate(ctx, *channel, key, target.ModelName)
		totalDuration += result.DurationMS
		if result.Success || !shouldTryNextKey(result) {
			break
		}
	}
	result.DurationMS = totalDuration
	status := model.ChannelModelHealthFailed
	if result.Success {
		status = model.ChannelModelHealthSuccess
	}
	now := time.Now()
	return save(ctx, target, status, result, usedKey, configFingerprint, &now)
}

func containsModel(channel model.Channel, name string) bool {
	for _, raw := range []string{channel.Model, channel.CustomModel} {
		for _, configured := range strings.Split(raw, ",") {
			if strings.TrimSpace(configured) == name {
				return true
			}
		}
	}
	return false
}

func enabledKeys(input []model.ChannelKey) []model.ChannelKey {
	keys := make([]model.ChannelKey, 0, len(input))
	for _, key := range input {
		if key.Enabled && strings.TrimSpace(key.ChannelKey) != "" {
			keys = append(keys, key)
		}
	}
	sort.SliceStable(keys, func(left, right int) bool {
		if keys[left].TotalCost == keys[right].TotalCost {
			return keys[left].ID < keys[right].ID
		}
		return keys[left].TotalCost < keys[right].TotalCost
	})
	return keys
}

func shouldTryNextKey(result grouphealth.ProbeResult) bool {
	if result.HTTPStatus == 401 || result.HTTPStatus == 403 || result.HTTPStatus == 429 {
		return true
	}
	message := strings.ToLower(result.ErrorMessage)
	for _, marker := range []string{"invalid api key", "unauthorized", "forbidden", "insufficient quota", "rate limit"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func saveFailure(ctx context.Context, target model.ChannelModelHealthTarget, message, configFingerprint string) error {
	now := time.Now()
	return save(ctx, target, model.ChannelModelHealthFailed, grouphealth.ProbeResult{ErrorMessage: message}, model.ChannelKey{}, configFingerprint, &now)
}

func save(ctx context.Context, target model.ChannelModelHealthTarget, status model.ChannelModelHealthStatus, result grouphealth.ProbeResult, key model.ChannelKey, configFingerprint string, checkedAt *time.Time) error {
	row := model.ChannelModelHealth{
		ChannelID: target.ChannelID, ModelName: target.ModelName, Status: status,
		HTTPStatus: result.HTTPStatus, DurationMS: result.DurationMS, ErrorMessage: result.ErrorMessage,
		ChannelKeyID: key.ID, KeyRemark: key.Remark, CheckedAt: checkedAt, ConfigFingerprint: configFingerprint,
	}
	return db.GetDB().WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "channel_id"}, {Name: "model_name"}},
		DoUpdates: clause.AssignmentColumns([]string{"status", "http_status", "duration_ms", "error_message", "channel_key_id", "key_remark", "checked_at", "config_fingerprint", "updated_at"}),
	}).Create(&row).Error
}

func Preview(ctx context.Context, targets []model.ChannelModelHealthTarget) ([]model.ChannelModelGroupPreviewItem, error) {
	items, err := op.ChannelModelGroupPreview(ctx, targets)
	if err != nil {
		return nil, err
	}
	health, err := Query(ctx, targets)
	if err != nil {
		return nil, err
	}
	healthByTarget := make(map[string]*model.ChannelModelHealth, len(health))
	for index := range health {
		healthByTarget[targetKey(model.ChannelModelHealthTarget{ChannelID: health[index].ChannelID, ModelName: health[index].ModelName})] = &health[index]
	}
	for index := range items {
		items[index].Health = healthByTarget[targetKey(model.ChannelModelHealthTarget{ChannelID: items[index].ChannelID, ModelName: items[index].ModelName})]
		sort.SliceStable(items[index].Candidates, func(left, right int) bool {
			rank := map[string]int{"exact": 0, "regex": 1, "fuzzy": 2}
			return rank[items[index].Candidates[left].Reason] < rank[items[index].Candidates[right].Reason]
		})
	}
	return items, nil
}

func Apply(ctx context.Context, req model.ChannelModelGroupApplyRequest) (*model.ChannelModelGroupApplyResult, error) {
	if len(req.Items) == 0 {
		return nil, applyValidationError("no group items selected")
	}
	targets := make([]model.ChannelModelHealthTarget, 0, len(req.Items))
	for index := range req.Items {
		req.Items[index].ModelName = strings.TrimSpace(req.Items[index].ModelName)
		req.Items[index].CreateGroupName = strings.TrimSpace(req.Items[index].CreateGroupName)
		item := req.Items[index]
		target := model.ChannelModelHealthTarget{ChannelID: item.ChannelID, ModelName: strings.TrimSpace(item.ModelName)}
		channel, err := op.ChannelGet(target.ChannelID, ctx)
		if err != nil {
			return nil, applyValidationError(fmt.Sprintf("channel %d not found", target.ChannelID))
		}
		if !containsModel(*channel, target.ModelName) {
			return nil, applyValidationError(fmt.Sprintf("model %s is not configured on channel %d", target.ModelName, target.ChannelID))
		}
		targets = append(targets, target)
	}
	rows, err := Query(ctx, targets)
	if err != nil {
		return nil, err
	}
	byTarget := make(map[string]model.ChannelModelHealth, len(rows))
	for _, row := range rows {
		byTarget[targetKey(model.ChannelModelHealthTarget{ChannelID: row.ChannelID, ModelName: row.ModelName})] = row
	}
	for _, item := range req.Items {
		if item.ForceUnhealthy {
			continue
		}
		row, ok := byTarget[targetKey(model.ChannelModelHealthTarget{ChannelID: item.ChannelID, ModelName: item.ModelName})]
		if !ok || row.Status != model.ChannelModelHealthSuccess {
			return nil, applyValidationError(fmt.Sprintf("model %s does not have a fresh healthy result", item.ModelName))
		}
	}
	return op.ChannelModelGroupApply(ctx, req)
}

func applyValidationError(message string) error {
	return apperror.New(apperror.CodeCommonValidationFailed, message).WithStatus(http.StatusBadRequest)
}
