package grouphealth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func setupGroupHealthTestDB(t *testing.T) context.Context {
	t.Helper()

	if dbpkg.GetDB() != nil {
		_ = dbpkg.Close()
	}

	dbPath := filepath.Join(t.TempDir(), "octopus-group-health-test.db")
	if err := dbpkg.InitDB("sqlite", dbPath, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("InitCache failed: %v", err)
	}
	t.Cleanup(func() {
		_ = dbpkg.Close()
	})

	return context.Background()
}

func TestExcludeAndRecoverFailedGroupItem(t *testing.T) {
	ctx := setupGroupHealthTestDB(t)
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, `{"error":"down"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed"}`))
	}))
	defer server.Close()

	channel := &model.Channel{Name: "recoverable", Type: outbound.OutboundTypeOpenAIResponse, Enabled: true, BaseUrls: []model.BaseUrl{{URL: server.URL + "/v1"}}, Model: "probe-model", Keys: []model.ChannelKey{{Enabled: true, ChannelKey: "sk-test"}}}
	if err := op.ChannelCreate(channel, ctx); err != nil {
		t.Fatal(err)
	}
	group := &model.Group{Name: "recoverable-group", Mode: model.GroupModeFailover}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatal(err)
	}
	item := &model.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: "probe-model"}
	if err := op.GroupItemAdd(item, ctx); err != nil {
		t.Fatal(err)
	}

	service := NewService(op.NewGroupHealthRepository(), &Prober{CandidateTimeout: 2 * time.Second})
	if err := service.RunGroupHealth(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	view, err := service.GetGroupHealthViewByID(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	attemptID := view.Latest.Attempts[0].ID
	if _, err := service.ExcludeLatestFailures(ctx, group.ID, false); err == nil {
		t.Fatal("expected batch last-active-item conflict")
	}
	view, err = service.ExcludeLatestFailures(ctx, group.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if view.ActiveItemCount != 0 || len(view.ExcludedItems) != 1 || view.Latest.Attempts[0].MembershipState != model.GroupHealthMembershipStateExcluded {
		t.Fatalf("unexpected excluded view: %#v", view)
	}
	if repeated, repeatErr := service.ExcludeAttempt(ctx, group.ID, attemptID, true); repeatErr != nil || repeated.ActiveItemCount != 0 || len(repeated.ExcludedItems) != 1 {
		t.Fatalf("repeated exclusion was not idempotent: view=%#v err=%v", repeated, repeatErr)
	}
	routed, err := op.GroupGetEnabledMap(group.Name, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(routed.Items) != 0 {
		t.Fatalf("excluded item remained routable: %#v", routed.Items)
	}

	failedRecovery, err := service.RestoreItem(ctx, group.ID, item.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if failedRecovery.Restored || failedRecovery.Probe.Success {
		t.Fatalf("failed probe restored item: %#v", failedRecovery)
	}
	healthy.Store(true)
	recovered, err := service.RestoreItem(ctx, group.ID, item.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Restored || !recovered.Probe.Success || recovered.ActiveItemCount != 1 {
		t.Fatalf("unexpected recovery: %#v", recovered)
	}
	if _, err := service.ExcludeAttempt(ctx, group.ID, attemptID, true); err != nil {
		t.Fatalf("exclude before force restore: %v", err)
	}
	forced, err := service.RestoreItem(ctx, group.ID, item.ID, true)
	if err != nil || !forced.Restored || forced.ActiveItemCount != 1 {
		t.Fatalf("unexpected force restore: result=%#v err=%v", forced, err)
	}
}

func TestBatchExcludeFailuresAndRestoreOnlyHealthyItems(t *testing.T) {
	ctx := setupGroupHealthTestDB(t)
	var firstHealthy atomic.Bool
	newServer := func(healthy func() bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !healthy() {
				http.Error(w, `{"error":"down"}`, http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed"}`))
		}))
	}
	firstServer := newServer(firstHealthy.Load)
	defer firstServer.Close()
	secondServer := newServer(func() bool { return false })
	defer secondServer.Close()
	healthyServer := newServer(func() bool { return true })
	defer healthyServer.Close()

	createChannel := func(name, baseURL string) *model.Channel {
		channel := &model.Channel{Name: name, Type: outbound.OutboundTypeOpenAIResponse, Enabled: true, BaseUrls: []model.BaseUrl{{URL: baseURL + "/v1"}}, Model: "batch-model", Keys: []model.ChannelKey{{Enabled: true, ChannelKey: "sk-test"}}}
		if err := op.ChannelCreate(channel, ctx); err != nil {
			t.Fatalf("ChannelCreate %s: %v", name, err)
		}
		return channel
	}
	channels := []*model.Channel{
		createChannel("batch-failed-first", firstServer.URL),
		createChannel("batch-failed-second", secondServer.URL),
		createChannel("batch-healthy", healthyServer.URL),
	}
	group := &model.Group{Name: "batch-recovery-group", Mode: model.GroupModeFailover}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatal(err)
	}
	for index, channel := range channels {
		if err := op.GroupItemAdd(&model.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: "batch-model", Priority: index + 1, Weight: 1}, ctx); err != nil {
			t.Fatal(err)
		}
	}

	service := NewService(op.NewGroupHealthRepository(), &Prober{CandidateTimeout: 2 * time.Second})
	if err := service.RunGroupHealth(ctx, group.ID, model.GroupHealthProbeModeFull); err != nil {
		t.Fatal(err)
	}
	view, err := service.ExcludeLatestFailures(ctx, group.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if view.ActiveItemCount != 1 || len(view.ExcludedItems) != 2 {
		t.Fatalf("expected 2 failed items excluded and 1 active, got %#v", view)
	}

	firstHealthy.Store(true)
	batch, err := service.ProbeAndRestoreExcluded(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Total != 2 || batch.RestoredCount != 1 || batch.FailedCount != 1 || batch.ActiveItemCount != 2 {
		t.Fatalf("unexpected batch recovery result: %#v", batch)
	}
	view, err = service.GetGroupHealthViewByID(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.ActiveItemCount != 2 || len(view.ExcludedItems) != 1 || view.ExcludedItems[0].ChannelID != channels[1].ID {
		t.Fatalf("only the still-failing item should remain excluded: %#v", view)
	}
}

func TestRunGroupHealthFailoverDoesNotMutateRuntimeStats(t *testing.T) {
	ctx := setupGroupHealthTestDB(t)

	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"upstream unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer firstServer.Close()

	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed"}`))
	}))
	defer secondServer.Close()

	firstChannel := &model.Channel{
		Name:     "group-health-first",
		Type:     outbound.OutboundTypeOpenAIResponse,
		Enabled:  true,
		BaseUrls: []model.BaseUrl{{URL: firstServer.URL + "/v1"}},
		Model:    "probe-model",
		Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "sk-first", Remark: "first"}},
	}
	if err := op.ChannelCreate(firstChannel, ctx); err != nil {
		t.Fatalf("ChannelCreate first failed: %v", err)
	}

	secondChannel := &model.Channel{
		Name:     "group-health-second",
		Type:     outbound.OutboundTypeOpenAIResponse,
		Enabled:  true,
		BaseUrls: []model.BaseUrl{{URL: secondServer.URL + "/v1"}},
		Model:    "probe-model",
		Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "sk-second", Remark: "second"}},
	}
	if err := op.ChannelCreate(secondChannel, ctx); err != nil {
		t.Fatalf("ChannelCreate second failed: %v", err)
	}

	group := &model.Group{Name: "probe-group", Mode: model.GroupModeFailover}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	if err := op.GroupItemAdd(&model.GroupItem{GroupID: group.ID, ChannelID: firstChannel.ID, ModelName: "probe-model", Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd first failed: %v", err)
	}
	if err := op.GroupItemAdd(&model.GroupItem{GroupID: group.ID, ChannelID: secondChannel.ID, ModelName: "probe-model", Priority: 2, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd second failed: %v", err)
	}

	statsBefore := op.StatsTotalGet()
	logsBefore, err := op.RelayLogList(ctx, nil, nil, nil, 1, 100)
	if err != nil {
		t.Fatalf("RelayLogList failed: %v", err)
	}

	service := NewService(op.NewGroupHealthRepository(), &Prober{CandidateTimeout: 5 * time.Second})
	if err := service.RunGroupHealth(ctx, group.ID); err != nil {
		t.Fatalf("RunGroupHealth failed: %v", err)
	}

	view, err := service.GetGroupHealthViewByID(ctx, group.ID)
	if err != nil {
		t.Fatalf("GetGroupHealthViewByID failed: %v", err)
	}
	if view.Latest == nil {
		t.Fatal("expected latest snapshot")
	}
	if view.Latest.Status != model.GroupHealthStatusPartial {
		t.Fatalf("expected partial status, got %s", view.Latest.Status)
	}
	if len(view.Latest.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(view.Latest.Attempts))
	}
	if view.Latest.Attempts[0].Status != model.GroupHealthAttemptStatusFailed {
		t.Fatalf("expected first attempt failed, got %s", view.Latest.Attempts[0].Status)
	}
	if view.Latest.Attempts[1].Status != model.GroupHealthAttemptStatusSuccess {
		t.Fatalf("expected second attempt success, got %s", view.Latest.Attempts[1].Status)
	}
	if view.Latest.SuccessfulChannelID == nil || *view.Latest.SuccessfulChannelID != secondChannel.ID {
		t.Fatalf("expected successful channel %d, got %#v", secondChannel.ID, view.Latest.SuccessfulChannelID)
	}

	statsAfter := op.StatsTotalGet()
	if statsAfter != statsBefore {
		t.Fatalf("expected stats total unchanged, before=%+v after=%+v", statsBefore, statsAfter)
	}

	logsAfter, err := op.RelayLogList(ctx, nil, nil, nil, 1, 100)
	if err != nil {
		t.Fatalf("RelayLogList failed after run: %v", err)
	}
	if len(logsAfter) != len(logsBefore) {
		t.Fatalf("expected relay log count unchanged, before=%d after=%d", len(logsBefore), len(logsAfter))
	}

	reloadedFirst, err := op.ChannelGet(firstChannel.ID, ctx)
	if err != nil {
		t.Fatalf("ChannelGet first failed: %v", err)
	}
	reloadedSecond, err := op.ChannelGet(secondChannel.ID, ctx)
	if err != nil {
		t.Fatalf("ChannelGet second failed: %v", err)
	}
	if reloadedFirst.Keys[0].TotalCost != 0 || reloadedSecond.Keys[0].TotalCost != 0 {
		t.Fatalf("expected key total cost unchanged")
	}
	if reloadedFirst.Keys[0].StatusCode != 0 || reloadedSecond.Keys[0].StatusCode != 0 {
		t.Fatalf("expected key status code unchanged")
	}
	if reloadedFirst.Keys[0].LastUseTimeStamp != 0 || reloadedSecond.Keys[0].LastUseTimeStamp != 0 {
		t.Fatalf("expected key last use timestamp unchanged")
	}
}

func TestRunGroupHealthReturnsAlreadyRunning(t *testing.T) {
	ctx := setupGroupHealthTestDB(t)

	group := &model.Group{Name: "running-group", Mode: model.GroupModeFailover}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}

	repo := op.NewGroupHealthRepository()
	if _, err := repo.CreateRunningSnapshot(ctx, *group, model.GroupHealthProbeModeStandard); err != nil {
		t.Fatalf("CreateRunningSnapshot failed: %v", err)
	}

	service := NewService(repo, nil)
	err := service.RunGroupHealth(ctx, group.ID)
	if err == nil {
		t.Fatal("expected ErrGroupHealthAlreadyRunning")
	}
	if !errors.Is(err, ErrGroupHealthAlreadyRunning) {
		t.Fatalf("expected ErrGroupHealthAlreadyRunning, got %v", err)
	}
}

func TestRunGroupHealthFullProbeDoesNotSkipRemainingFailoverCandidates(t *testing.T) {
	ctx := setupGroupHealthTestDB(t)

	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_first","object":"response","status":"completed"}`))
	}))
	defer firstServer.Close()

	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"upstream unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer secondServer.Close()

	firstChannel := &model.Channel{
		Name:     "group-health-full-first",
		Type:     outbound.OutboundTypeOpenAIResponse,
		Enabled:  true,
		BaseUrls: []model.BaseUrl{{URL: firstServer.URL + "/v1"}},
		Model:    "probe-model",
		Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "sk-full-first", Remark: "first"}},
	}
	if err := op.ChannelCreate(firstChannel, ctx); err != nil {
		t.Fatalf("ChannelCreate first failed: %v", err)
	}

	secondChannel := &model.Channel{
		Name:     "group-health-full-second",
		Type:     outbound.OutboundTypeOpenAIResponse,
		Enabled:  true,
		BaseUrls: []model.BaseUrl{{URL: secondServer.URL + "/v1"}},
		Model:    "probe-model",
		Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "sk-full-second", Remark: "second"}},
	}
	if err := op.ChannelCreate(secondChannel, ctx); err != nil {
		t.Fatalf("ChannelCreate second failed: %v", err)
	}

	group := &model.Group{Name: "probe-full-group", Mode: model.GroupModeFailover}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	if err := op.GroupItemAdd(&model.GroupItem{GroupID: group.ID, ChannelID: firstChannel.ID, ModelName: "probe-model", Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd first failed: %v", err)
	}
	if err := op.GroupItemAdd(&model.GroupItem{GroupID: group.ID, ChannelID: secondChannel.ID, ModelName: "probe-model", Priority: 2, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd second failed: %v", err)
	}

	service := NewService(op.NewGroupHealthRepository(), &Prober{CandidateTimeout: 5 * time.Second})
	if err := service.RunGroupHealth(ctx, group.ID, model.GroupHealthProbeModeFull); err != nil {
		t.Fatalf("RunGroupHealth full failed: %v", err)
	}

	view, err := service.GetGroupHealthViewByID(ctx, group.ID)
	if err != nil {
		t.Fatalf("GetGroupHealthViewByID failed: %v", err)
	}
	if view.Latest == nil {
		t.Fatal("expected latest snapshot")
	}
	if view.Latest.ProbeMode != model.GroupHealthProbeModeFull {
		t.Fatalf("expected full probe mode, got %s", view.Latest.ProbeMode)
	}
	if view.Latest.Status != model.GroupHealthStatusPartial {
		t.Fatalf("expected partial status, got %s", view.Latest.Status)
	}
	if len(view.Latest.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(view.Latest.Attempts))
	}
	if view.Latest.Attempts[0].Status != model.GroupHealthAttemptStatusSuccess {
		t.Fatalf("expected first attempt success, got %s", view.Latest.Attempts[0].Status)
	}
	if view.Latest.Attempts[1].Status != model.GroupHealthAttemptStatusFailed {
		t.Fatalf("expected second attempt failed, got %s", view.Latest.Attempts[1].Status)
	}
	if view.Latest.Attempts[1].HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("expected second attempt http status %d, got %d", http.StatusServiceUnavailable, view.Latest.Attempts[1].HTTPStatus)
	}
}
