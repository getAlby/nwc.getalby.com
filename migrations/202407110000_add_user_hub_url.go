package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Save user's new hub URL so it only has to be requested once on auth
var _202407110000_user_hub_url = &gormigrate.Migration{
	ID: "_202407110000_user_hub_url",
	Migrate: func(tx *gorm.DB) error {
		return tx.Exec("ALTER TABLE users ADD COLUMN hub_url TEXT").Error
	},
	Rollback: func(tx *gorm.DB) error {
		return nil
	},
}
