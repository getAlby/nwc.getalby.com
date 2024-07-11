package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Create a composite index to improve performance of finding the latest nostr event for an app
var _202407110000_user_hub_url = &gormigrate.Migration{
	ID: "_202407110000_user_hub_url",
	Migrate: func(tx *gorm.DB) error {
		return tx.Exec("ALTER TABLE users ADD COLUMN hub_url TEXT").Error
	},
	Rollback: func(tx *gorm.DB) error {
		return nil
	},
}
