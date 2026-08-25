package op

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

func TestGroupParamOverridePersistsThroughPresetLifecycle(t *testing.T) {
	ctx := setupSiteOpTestDB(t)
	override := `{"temperature":0.4,"nested":{"level":1}}`
	group := &model.Group{Name: "group-override-lifecycle", Mode: model.GroupModeFailover, ParamOverride: &override}
	if err := GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	got, err := GroupGet(group.ID, ctx)
	if err != nil {
		t.Fatalf("GroupGet failed: %v", err)
	}
	if got.ParamOverride == nil || *got.ParamOverride != override {
		t.Fatalf("group override was not persisted: %#v", got.ParamOverride)
	}

	preset, err := GroupPresetCreate(group.ID, "override preset", ctx)
	if err != nil {
		t.Fatalf("GroupPresetCreate failed: %v", err)
	}
	if preset.ParamOverride == nil || *preset.ParamOverride != override {
		t.Fatalf("preset snapshot lost group override: %#v", preset.ParamOverride)
	}

	clone, err := GroupPresetClone(preset.ID, "override clone", ctx)
	if err != nil {
		t.Fatalf("GroupPresetClone failed: %v", err)
	}
	if clone.ParamOverride == nil || *clone.ParamOverride != override {
		t.Fatalf("preset clone lost group override: %#v", clone.ParamOverride)
	}

	updated := `{"temperature":0.9,"nested":{"level":2}}`
	if _, err := GroupPresetUpdate(clone.ID, &model.GroupPresetUpdateRequest{ParamOverride: &updated}, ctx); err != nil {
		t.Fatalf("GroupPresetUpdate failed: %v", err)
	}
	if err := GroupPresetActivate(clone.ID, ctx); err != nil {
		t.Fatalf("GroupPresetActivate failed: %v", err)
	}
	got, err = GroupGet(group.ID, ctx)
	if err != nil {
		t.Fatalf("GroupGet after activate failed: %v", err)
	}
	if got.ParamOverride == nil || *got.ParamOverride != updated {
		t.Fatalf("active preset override was not mirrored to group: %#v", got.ParamOverride)
	}
}

func TestGroupParamOverrideRejectsProtectedFields(t *testing.T) {
	ctx := setupSiteOpTestDB(t)
	protected := `{"model":"must-not-change"}`
	if err := GroupCreate(&model.Group{Name: "group-override-protected", Mode: model.GroupModeFailover, ParamOverride: &protected}, ctx); err == nil {
		t.Fatal("expected protected group override to be rejected")
	}
}
