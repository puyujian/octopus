package grouphealth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/apperror"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"gorm.io/gorm"
)

var ErrGroupHealthAlreadyRunning = errors.New("group health check already running")

type Repository interface {
	CreateRunningSnapshot(ctx context.Context, group model.Group, probeMode model.GroupHealthProbeMode) (*model.GroupHealthSnapshot, error)
	AppendAttempt(ctx context.Context, snapshotID int, attempt model.GroupHealthAttempt) error
	FinishSnapshot(ctx context.Context, snapshotID int, status model.GroupHealthStatus, successfulChannelID *int, durationMS int64, message string, finishedAt time.Time) error
	GetLatestSnapshotByGroupID(ctx context.Context, groupID int) (*model.GroupHealthSnapshot, error)
	GetRunningSnapshotByGroupID(ctx context.Context, groupID int) (*model.GroupHealthSnapshot, error)
	ListGroupHealthViews(ctx context.Context) ([]model.GroupHealthGroupView, error)
	GetGroupHealthViewByID(ctx context.Context, groupID int) (*model.GroupHealthGroupView, error)
	GetAttemptByID(ctx context.Context, attemptID int) (*model.GroupHealthAttempt, error)
	GetSnapshotByID(ctx context.Context, snapshotID int) (*model.GroupHealthSnapshot, error)
}

type Service struct {
	repo   Repository
	prober *Prober
}

var runLocks sync.Map

func NewService(repo Repository, prober *Prober) *Service {
	if repo == nil {
		repo = op.NewGroupHealthRepository()
	}
	if prober == nil {
		prober = NewProber()
	}
	return &Service{
		repo:   repo,
		prober: prober,
	}
}

func tryLockGroup(groupID int) (func(), bool) {
	value, _ := runLocks.LoadOrStore(groupID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	if !lock.TryLock() {
		return nil, false
	}
	return func() {
		lock.Unlock()
	}, true
}

// normalizeProbeMode returns the effective probe mode from a prioritized list.
// An empty list defaults to model.GroupHealthProbeModeStandard, and only the
// first element is considered. model.GroupHealthProbeModeFull is honored only
// when it appears first; all other cases fall back to Standard semantics.
func normalizeProbeMode(probeModes []model.GroupHealthProbeMode) model.GroupHealthProbeMode {
	if len(probeModes) == 0 {
		return model.GroupHealthProbeModeStandard
	}
	if probeModes[0] == model.GroupHealthProbeModeFull {
		return model.GroupHealthProbeModeFull
	}
	return model.GroupHealthProbeModeStandard
}

func resolveChannelName(ctx context.Context, channelID int) string {
	channel, err := op.ChannelGet(channelID, ctx)
	if err != nil {
		return fmt.Sprintf("channel-%d", channelID)
	}
	return channel.Name
}

func (s *Service) RunGroupHealth(ctx context.Context, groupID int, probeModes ...model.GroupHealthProbeMode) error {
	unlock, ok := tryLockGroup(groupID)
	if !ok {
		return ErrGroupHealthAlreadyRunning
	}
	defer unlock()

	if _, err := s.repo.GetRunningSnapshotByGroupID(ctx, groupID); err == nil {
		return ErrGroupHealthAlreadyRunning
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	group, err := op.GroupGet(groupID, ctx)
	if err != nil {
		return err
	}

	probeMode := normalizeProbeMode(probeModes)

	snapshot, err := s.repo.CreateRunningSnapshot(ctx, *group, probeMode)
	if err != nil {
		return err
	}

	items := make([]model.GroupItem, 0, len(group.Items))
	for _, item := range group.Items {
		if item.ExcludedAt == nil {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority < items[j].Priority
		}
		if items[i].Weight != items[j].Weight {
			return items[i].Weight > items[j].Weight
		}
		if items[i].ChannelID != items[j].ChannelID {
			return items[i].ChannelID < items[j].ChannelID
		}
		return items[i].ID < items[j].ID
	})

	var successfulChannelID *int
	message := "all candidates failed"
	stopAfterSuccess := group.Mode == model.GroupModeFailover && probeMode != model.GroupHealthProbeModeFull
	successFound := false
	firstSuccessIndex := -1
	attemptedCount := 0
	successCount := 0

	for index, item := range items {
		channel, err := op.ChannelGet(item.ChannelID, ctx)
		if err != nil {
			attemptedCount++
			appendErr := s.repo.AppendAttempt(ctx, snapshot.ID, model.GroupHealthAttempt{
				GroupItemID:  item.ID,
				ChannelID:    item.ChannelID,
				ChannelName:  fmt.Sprintf("channel-%d", item.ChannelID),
				ModelName:    item.ModelName,
				Priority:     item.Priority,
				Weight:       item.Weight,
				Status:       model.GroupHealthAttemptStatusFailed,
				ErrorMessage: fmt.Sprintf("failed to load channel: %v", err),
			})
			if appendErr != nil {
				return appendErr
			}
			continue
		}

		usedKey := channel.GetChannelKey()
		if usedKey.ID == 0 || strings.TrimSpace(usedKey.ChannelKey) == "" {
			attemptedCount++
			appendErr := s.repo.AppendAttempt(ctx, snapshot.ID, model.GroupHealthAttempt{
				GroupItemID:  item.ID,
				ChannelID:    item.ChannelID,
				ChannelName:  channel.Name,
				ModelName:    item.ModelName,
				Priority:     item.Priority,
				Weight:       item.Weight,
				Status:       model.GroupHealthAttemptStatusFailed,
				ErrorMessage: "no available key",
			})
			if appendErr != nil {
				return appendErr
			}
			continue
		}

		result := s.prober.RunCandidateWithGroupOverride(ctx, *channel, usedKey, item.ModelName, group.ParamOverride)
		attemptedCount++
		attempt := model.GroupHealthAttempt{
			GroupItemID:  item.ID,
			ChannelID:    item.ChannelID,
			ChannelName:  channel.Name,
			ChannelKeyID: usedKey.ID,
			KeyRemark:    usedKey.Remark,
			ModelName:    item.ModelName,
			Priority:     item.Priority,
			Weight:       item.Weight,
			HTTPStatus:   result.HTTPStatus,
			DurationMS:   result.DurationMS,
			ErrorMessage: result.ErrorMessage,
		}
		if result.Success {
			attempt.Status = model.GroupHealthAttemptStatusSuccess
		} else {
			attempt.Status = model.GroupHealthAttemptStatusFailed
		}
		if err := s.repo.AppendAttempt(ctx, snapshot.ID, attempt); err != nil {
			return err
		}

		if result.Success {
			successFound = true
			successCount++
			if firstSuccessIndex == -1 {
				firstSuccessIndex = index
				successfulChannelID = &item.ChannelID
			}
			if stopAfterSuccess {
				for _, skipped := range items[index+1:] {
					channelName := fmt.Sprintf("channel-%d", skipped.ChannelID)
					if skippedChannel, getErr := op.ChannelGet(skipped.ChannelID, ctx); getErr == nil {
						channelName = skippedChannel.Name
					}
					if err := s.repo.AppendAttempt(ctx, snapshot.ID, model.GroupHealthAttempt{
						GroupItemID: skipped.ID,
						ChannelID:   skipped.ChannelID,
						ChannelName: channelName,
						ModelName:   skipped.ModelName,
						Priority:    skipped.Priority,
						Weight:      skipped.Weight,
						Status:      model.GroupHealthAttemptStatusSkipped,
					}); err != nil {
						return err
					}
				}
				break
			}
		}
	}

	finalStatus := model.GroupHealthStatusFailed
	if !successFound && len(items) == 0 {
		message = "group has no items"
	} else if successFound {
		successChannelName := resolveChannelName(ctx, items[firstSuccessIndex].ChannelID)
		switch {
		case stopAfterSuccess && firstSuccessIndex == 0:
			finalStatus = model.GroupHealthStatusSuccess
			message = fmt.Sprintf("candidate %s succeeded", successChannelName)
		case stopAfterSuccess:
			finalStatus = model.GroupHealthStatusPartial
			message = fmt.Sprintf("candidate %s succeeded after failover", successChannelName)
		case successCount == attemptedCount:
			finalStatus = model.GroupHealthStatusSuccess
			message = fmt.Sprintf("all %d candidates succeeded", successCount)
		default:
			finalStatus = model.GroupHealthStatusPartial
			message = fmt.Sprintf("%d/%d candidates succeeded", successCount, attemptedCount)
		}
	}

	finishedAt := time.Now()
	durationMS := finishedAt.Sub(snapshot.StartedAt).Milliseconds()
	return s.repo.FinishSnapshot(ctx, snapshot.ID, finalStatus, successfulChannelID, durationMS, message, finishedAt)
}

func (s *Service) RunAllGroupHealth(ctx context.Context, maxConcurrency int, probeModes ...model.GroupHealthProbeMode) {
	if maxConcurrency <= 0 {
		maxConcurrency = 2
	}
	probeMode := normalizeProbeMode(probeModes)
	groups, err := op.GroupList(ctx)
	if err != nil {
		return
	}
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	for _, group := range groups {
		groupID := group.ID
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_ = s.RunGroupHealth(ctx, groupID, probeMode)
		}()
	}
	wg.Wait()
}

func (s *Service) ListGroupHealthViews(ctx context.Context) ([]model.GroupHealthGroupView, error) {
	return s.repo.ListGroupHealthViews(ctx)
}

func (s *Service) GetGroupHealthViewByID(ctx context.Context, groupID int) (*model.GroupHealthGroupView, error) {
	return s.repo.GetGroupHealthViewByID(ctx, groupID)
}

func (s *Service) GetRunningSnapshotByGroupID(ctx context.Context, groupID int) (*model.GroupHealthSnapshot, error) {
	return s.repo.GetRunningSnapshotByGroupID(ctx, groupID)
}

const (
	CodeAttemptNotFailed = "group_health.attempt_not_failed"
	CodeAttemptStale     = "group_health.attempt_stale"
	CodeAttemptMismatch  = "group_health.attempt_mismatch"
	CodeAlreadyRunning   = "group_health.already_running"
	CodeItemNotExcluded  = "group_health.item_not_excluded"
)

func conflict(code, message string) error {
	return apperror.New(code, message).WithStatus(409)
}

func (s *Service) ExcludeAttempt(ctx context.Context, groupID, attemptID int, allowEmpty bool) (*model.GroupHealthGroupView, error) {
	unlock, ok := tryLockGroup(groupID)
	if !ok {
		return nil, conflict(CodeAlreadyRunning, ErrGroupHealthAlreadyRunning.Error())
	}
	defer unlock()
	if _, err := s.repo.GetRunningSnapshotByGroupID(ctx, groupID); err == nil {
		return nil, conflict(CodeAlreadyRunning, ErrGroupHealthAlreadyRunning.Error())
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	attempt, err := s.repo.GetAttemptByID(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	attemptSnapshot, err := s.repo.GetSnapshotByID(ctx, attempt.SnapshotID)
	if err != nil {
		return nil, err
	}
	if attemptSnapshot.GroupID != groupID {
		return nil, conflict(CodeAttemptMismatch, "attempt does not belong to this group")
	}
	latest, err := s.repo.GetLatestSnapshotByGroupID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if attempt.SnapshotID != latest.ID {
		return nil, conflict(CodeAttemptStale, "only the latest health result can be excluded")
	}
	if attempt.Status != model.GroupHealthAttemptStatusFailed {
		return nil, conflict(CodeAttemptNotFailed, "only failed attempts can be excluded")
	}
	if _, _, err := op.GroupHealthExcludeItem(ctx, groupID, attempt.GroupItemID, attempt.ID, allowEmpty); err != nil {
		return nil, err
	}
	return s.repo.GetGroupHealthViewByID(ctx, groupID)
}

// ExcludeLatestFailures atomically excludes every currently-active failed item
// from the latest health snapshot. Historical, skipped and successful attempts
// are never included.
func (s *Service) ExcludeLatestFailures(ctx context.Context, groupID int, allowEmpty bool) (*model.GroupHealthGroupView, error) {
	unlock, ok := tryLockGroup(groupID)
	if !ok {
		return nil, conflict(CodeAlreadyRunning, ErrGroupHealthAlreadyRunning.Error())
	}
	defer unlock()
	if _, err := s.repo.GetRunningSnapshotByGroupID(ctx, groupID); err == nil {
		return nil, conflict(CodeAlreadyRunning, ErrGroupHealthAlreadyRunning.Error())
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	latest, err := s.repo.GetLatestSnapshotByGroupID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	group, err := op.GroupGet(groupID, ctx)
	if err != nil {
		return nil, err
	}
	activeItemIDs := make(map[int]struct{}, len(group.Items))
	for _, item := range group.Items {
		if item.ExcludedAt == nil {
			activeItemIDs[item.ID] = struct{}{}
		}
	}
	attemptByItem := make(map[int]int)
	for _, attempt := range latest.Attempts {
		if attempt.Status != model.GroupHealthAttemptStatusFailed {
			continue
		}
		if _, active := activeItemIDs[attempt.GroupItemID]; active {
			attemptByItem[attempt.GroupItemID] = attempt.ID
		}
	}
	if _, _, err := op.GroupHealthExcludeItems(ctx, groupID, attemptByItem, allowEmpty); err != nil {
		return nil, err
	}
	return s.repo.GetGroupHealthViewByID(ctx, groupID)
}

func (s *Service) RestoreItem(ctx context.Context, groupID, itemID int, force bool) (*model.GroupHealthRecoveryResult, error) {
	unlock, ok := tryLockGroup(groupID)
	if !ok {
		return nil, conflict(CodeAlreadyRunning, ErrGroupHealthAlreadyRunning.Error())
	}
	defer unlock()
	if _, err := s.repo.GetRunningSnapshotByGroupID(ctx, groupID); err == nil {
		return nil, conflict(CodeAlreadyRunning, ErrGroupHealthAlreadyRunning.Error())
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	group, err := op.GroupGet(groupID, ctx)
	if err != nil {
		return nil, err
	}
	var item *model.GroupItem
	for i := range group.Items {
		if group.Items[i].ID == itemID {
			copyItem := group.Items[i]
			item = &copyItem
			break
		}
	}
	if item == nil {
		return nil, apperror.New(op.CodeGroupHealthItemNotFound, "group item not found").WithStatus(404)
	}
	if item.ExcludedAt == nil {
		return nil, conflict(CodeItemNotExcluded, "group item is not excluded")
	}
	result := &model.GroupHealthRecoveryResult{ItemID: itemID}
	if force {
		_, count, restoreErr := op.GroupHealthRestoreItem(ctx, groupID, itemID)
		if restoreErr != nil {
			return nil, restoreErr
		}
		result.Restored, result.ActiveItemCount = true, count
		return result, nil
	}
	channel, err := op.ChannelGet(item.ChannelID, ctx)
	if err != nil {
		result.Probe.ErrorMessage = fmt.Sprintf("failed to load channel: %v", err)
		return result, nil
	}
	if !channel.Enabled {
		result.Probe.ErrorMessage = "channel is disabled"
		return result, nil
	}
	usedKey := channel.GetChannelKey()
	if usedKey.ID == 0 || strings.TrimSpace(usedKey.ChannelKey) == "" {
		result.Probe.ErrorMessage = "no available key"
		return result, nil
	}
	probe := s.prober.RunCandidateWithGroupOverride(ctx, *channel, usedKey, item.ModelName, group.ParamOverride)
	result.Probe = model.GroupHealthRecoveryProbe{Success: probe.Success, HTTPStatus: probe.HTTPStatus, DurationMS: probe.DurationMS, ErrorMessage: probe.ErrorMessage}
	if !probe.Success {
		return result, nil
	}
	_, count, err := op.GroupHealthRestoreItem(ctx, groupID, itemID)
	if err != nil {
		return nil, err
	}
	result.Restored, result.ActiveItemCount = true, count
	return result, nil
}

// ProbeAndRestoreExcluded probes all currently excluded items and restores only
// the successful subset in one transaction. Failed/disabled/keyless items stay excluded.
func (s *Service) ProbeAndRestoreExcluded(ctx context.Context, groupID int) (*model.GroupHealthBatchRecoveryResult, error) {
	unlock, ok := tryLockGroup(groupID)
	if !ok {
		return nil, conflict(CodeAlreadyRunning, ErrGroupHealthAlreadyRunning.Error())
	}
	defer unlock()
	if _, err := s.repo.GetRunningSnapshotByGroupID(ctx, groupID); err == nil {
		return nil, conflict(CodeAlreadyRunning, ErrGroupHealthAlreadyRunning.Error())
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	group, err := op.GroupGet(groupID, ctx)
	if err != nil {
		return nil, err
	}
	excluded := make([]model.GroupItem, 0)
	activeCount := 0
	for _, item := range group.Items {
		if item.ExcludedAt != nil {
			excluded = append(excluded, item)
		} else {
			activeCount++
		}
	}
	batch := &model.GroupHealthBatchRecoveryResult{
		Total: len(excluded), ActiveItemCount: activeCount,
		Results: make([]model.GroupHealthRecoveryResult, len(excluded)),
	}
	if len(excluded) == 0 {
		return batch, nil
	}

	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for index := range excluded {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			item := excluded[index]
			result := model.GroupHealthRecoveryResult{ItemID: item.ID}
			channel, channelErr := op.ChannelGet(item.ChannelID, ctx)
			switch {
			case channelErr != nil:
				result.Probe.ErrorMessage = fmt.Sprintf("failed to load channel: %v", channelErr)
			case !channel.Enabled:
				result.Probe.ErrorMessage = "channel is disabled"
			default:
				usedKey := channel.GetChannelKey()
				if usedKey.ID == 0 || strings.TrimSpace(usedKey.ChannelKey) == "" {
					result.Probe.ErrorMessage = "no available key"
				} else {
					probe := s.prober.RunCandidateWithGroupOverride(ctx, *channel, usedKey, item.ModelName, group.ParamOverride)
					result.Probe = model.GroupHealthRecoveryProbe{Success: probe.Success, HTTPStatus: probe.HTTPStatus, DurationMS: probe.DurationMS, ErrorMessage: probe.ErrorMessage}
				}
			}
			batch.Results[index] = result
		}()
	}
	wg.Wait()

	restoreIDs := make([]int, 0, len(excluded))
	for i := range batch.Results {
		if batch.Results[i].Probe.Success {
			restoreIDs = append(restoreIDs, batch.Results[i].ItemID)
		}
	}
	if len(restoreIDs) > 0 {
		_, activeCount, restoreErr := op.GroupHealthRestoreItems(ctx, groupID, restoreIDs)
		if restoreErr != nil {
			return nil, restoreErr
		}
		batch.ActiveItemCount = activeCount
	}
	for i := range batch.Results {
		batch.Results[i].Restored = batch.Results[i].Probe.Success
		batch.Results[i].ActiveItemCount = batch.ActiveItemCount
		if batch.Results[i].Restored {
			batch.RestoredCount++
		}
	}
	batch.FailedCount = batch.Total - batch.RestoredCount
	return batch, nil
}
