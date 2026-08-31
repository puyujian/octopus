package channelmodel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/grouphealth"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func setupChannelModelTestDB(t *testing.T) context.Context {
	t.Helper()
	if dbpkg.GetDB() != nil {
		_ = dbpkg.Close()
	}
	activeTargets.Range(func(key, _ any) bool {
		activeTargets.Delete(key)
		return true
	})
	dbPath := filepath.Join(t.TempDir(), "octopus-channel-model-test.db")
	if err := dbpkg.InitDB("sqlite", dbPath, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("InitCache failed: %v", err)
	}
	t.Cleanup(func() { _ = dbpkg.Close() })
	return context.Background()
}

func createChannelModelTestChannel(t *testing.T, ctx context.Context, name, baseURL, models string, keys []model.ChannelKey) *model.Channel {
	t.Helper()
	channel := &model.Channel{
		Name: name, Type: outbound.OutboundTypeOpenAIChat, Enabled: true,
		BaseUrls: []model.BaseUrl{{URL: baseURL + "/v1"}}, Model: models, Keys: keys,
	}
	if err := op.ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}
	return channel
}

func TestRunTargetUsesRequestedModelAndFallsBackAcrossKeys(t *testing.T) {
	ctx := setupChannelModelTestDB(t)
	var requests atomic.Int32
	var modelsMu sync.Mutex
	seenModels := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode probe body: %v", err)
		}
		modelsMu.Lock()
		seenModels = append(seenModels, body.Model)
		modelsMu.Unlock()
		time.Sleep(15 * time.Millisecond)
		if r.Header.Get("Authorization") == "Bearer bad-key" {
			http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	channel := createChannelModelTestChannel(t, ctx, "fallback", server.URL, "model-a,model-b", []model.ChannelKey{
		{Enabled: true, ChannelKey: "bad-key", Remark: "bad", TotalCost: 0},
		{Enabled: true, ChannelKey: "good-key", Remark: "good", TotalCost: 1},
	})
	target := model.ChannelModelHealthTarget{ChannelID: channel.ID, ModelName: "model-b"}
	if err := runTarget(ctx, target); err != nil {
		t.Fatalf("runTarget failed: %v", err)
	}
	rows, err := Query(ctx, []model.ChannelModelHealthTarget{target})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != model.ChannelModelHealthSuccess || rows[0].KeyRemark != "good" {
		t.Fatalf("unexpected health row: %#v", rows)
	}
	if requests.Load() != 2 || rows[0].DurationMS < 20 {
		t.Fatalf("expected two attempts with aggregate duration, requests=%d duration=%d", requests.Load(), rows[0].DurationMS)
	}
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if len(seenModels) != 2 || seenModels[0] != "model-b" || seenModels[1] != "model-b" {
		t.Fatalf("probe did not preserve requested model: %#v", seenModels)
	}
}

func TestQueryMarksExpiredChangedAndInterruptedResults(t *testing.T) {
	ctx := setupChannelModelTestDB(t)
	channel := createChannelModelTestChannel(t, ctx, "freshness", "http://127.0.0.1:1", "fresh,changed,orphan", []model.ChannelKey{{Enabled: true, ChannelKey: "key"}})
	now := time.Now()

	fresh := model.ChannelModelHealthTarget{ChannelID: channel.ID, ModelName: "fresh"}
	if err := save(ctx, fresh, model.ChannelModelHealthSuccess, grouphealth.ProbeResult{Success: true}, channel.Keys[0], fingerprint(*channel), &now); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-healthTTL - time.Minute)
	expired := model.ChannelModelHealthTarget{ChannelID: channel.ID, ModelName: "changed"}
	if err := save(ctx, expired, model.ChannelModelHealthSuccess, grouphealth.ProbeResult{Success: true}, channel.Keys[0], fingerprint(*channel), &old); err != nil {
		t.Fatal(err)
	}
	orphan := model.ChannelModelHealthTarget{ChannelID: channel.ID, ModelName: "orphan"}
	if err := save(ctx, orphan, model.ChannelModelHealthRunning, grouphealth.ProbeResult{}, model.ChannelKey{}, fingerprint(*channel), nil); err != nil {
		t.Fatal(err)
	}

	rows, err := Query(ctx, []model.ChannelModelHealthTarget{fresh, expired, orphan})
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]model.ChannelModelHealthStatus{}
	for _, row := range rows {
		statuses[row.ModelName] = row.Status
	}
	if statuses["fresh"] != model.ChannelModelHealthSuccess || statuses["changed"] != model.ChannelModelHealthStale || statuses["orphan"] != model.ChannelModelHealthInterrupted {
		t.Fatalf("unexpected statuses: %#v", statuses)
	}

	if err := dbpkg.GetDB().WithContext(ctx).Model(&model.ChannelModelHealth{}).Where("channel_id = ? AND model_name = ?", channel.ID, "fresh").Updates(map[string]any{"config_fingerprint": "old", "checked_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	rows, err = Query(ctx, []model.ChannelModelHealthTarget{fresh})
	if err != nil || len(rows) != 1 || rows[0].Status != model.ChannelModelHealthStale {
		t.Fatalf("configuration change should stale result, rows=%#v err=%v", rows, err)
	}
}

func TestRunDeduplicatesConcurrentTarget(t *testing.T) {
	ctx := setupChannelModelTestDB(t)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	channel := createChannelModelTestChannel(t, ctx, "dedupe", server.URL, "model-a", []model.ChannelKey{{Enabled: true, ChannelKey: "key"}})
	target := model.ChannelModelHealthTarget{ChannelID: channel.ID, ModelName: "model-a"}
	if _, count, err := Run(ctx, []model.ChannelModelHealthTarget{target}); err != nil || count != 1 {
		t.Fatalf("first Run: count=%d err=%v", count, err)
	}
	if _, count, err := Run(ctx, []model.ChannelModelHealthTarget{target}); err == nil || count != 0 {
		t.Fatalf("duplicate Run should be rejected: count=%d err=%v", count, err)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for isActive(targetKey(target)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if isActive(targetKey(target)) {
		t.Fatal("background probe did not finish")
	}
}
