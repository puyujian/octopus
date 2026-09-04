package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 18,
		Up:      migrateOrdinaryChannelsToAutoRouting,
	})
}

func migrateOrdinaryChannelsToAutoRouting(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable(&model.Channel{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&model.Channel{}, "ModelRoutes") {
		if err := db.Migrator().AddColumn(&model.Channel{}, "ModelRoutes"); err != nil {
			return err
		}
	}

	boundChannelIDs := make(map[int]struct{})
	if db.Migrator().HasTable(&model.SiteChannelBinding{}) {
		var ids []int
		if err := db.Model(&model.SiteChannelBinding{}).Pluck("channel_id", &ids).Error; err != nil {
			return err
		}
		for _, id := range ids {
			boundChannelIDs[id] = struct{}{}
		}
	}

	var channels []model.Channel
	if err := db.Find(&channels).Error; err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		for i := range channels {
			channel := &channels[i]
			if _, managed := boundChannelIDs[channel.ID]; managed {
				continue
			}
			routes := channel.ModelRoutes.Normalize()
			if channel.Type != outbound.OutboundTypeAuto {
				if channel.Type.IsConcrete() {
					routes.FallbackType = channel.Type
				}
				channel.Type = outbound.OutboundTypeAuto
			}
			channel.ModelRoutes = routes.Normalize()
			if err := tx.Model(&model.Channel{}).Where("id = ?", channel.ID).
				Select("type", "model_routes").Updates(channel).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
