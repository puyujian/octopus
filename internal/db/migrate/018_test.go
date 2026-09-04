package migrate

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestMigrateOrdinaryChannelsToAutoRouting(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Channel{}, &model.ChannelKey{}, &model.SiteChannelBinding{}); err != nil {
		t.Fatal(err)
	}
	ordinary := model.Channel{Name: "ordinary", Type: outbound.OutboundTypeAnthropic}
	managed := model.Channel{Name: "managed", Type: outbound.OutboundTypeGemini}
	if err := db.Create(&ordinary).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&managed).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SiteChannelBinding{SiteID: 1, SiteAccountID: 1, GroupKey: "default", ChannelID: managed.ID}).Error; err != nil {
		t.Fatal(err)
	}

	if err := migrateOrdinaryChannelsToAutoRouting(db); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&ordinary, ordinary.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&managed, managed.ID).Error; err != nil {
		t.Fatal(err)
	}
	if ordinary.Type != outbound.OutboundTypeAuto || ordinary.ModelRoutes.FallbackType != outbound.OutboundTypeAnthropic {
		t.Fatalf("ordinary channel was not migrated correctly: type=%d routes=%+v", ordinary.Type, ordinary.ModelRoutes)
	}
	if managed.Type != outbound.OutboundTypeGemini {
		t.Fatalf("managed channel type changed: %d", managed.Type)
	}
}
