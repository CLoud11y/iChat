package utils

import (
	"fmt"
	"iChat/config"
	"iChat/models"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var DB *gorm.DB

func OpenMySQL(conf config.Config) (*gorm.DB, error) {
	// 初始化数据库连接
	mysqlConf := &conf.MYSQL
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		mysqlConf.User,
		mysqlConf.Password,
		mysqlConf.Host,
		mysqlConf.Port,
		mysqlConf.DbName,
	)
	MysqlConf := mysql.New(mysql.Config{DSN: dsn})
	GormConf := &gorm.Config{}

	db, err := gorm.Open(MysqlConf, GormConf)
	if err != nil {
		// GORM closes the pool on dialector initialization errors, but an
		// automatic Ping failure can also return a DB with an open pool.
		if db != nil {
			if sqlDB, poolErr := db.DB(); poolErr == nil {
				_ = sqlDB.Close()
			}
		}
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	return db, nil
}

// MigrateLegacy applies the transitional schema for cmd/migrate and isolated
// test fixtures. Business startup and connection construction never migrate.
// Versioned migrations for the reliability schema are introduced in P2.
func MigrateLegacy(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("migrate legacy: mysql is not initialized")
	}
	return db.AutoMigrate(&models.User{}, &models.Relation{}, &models.Group{}, &models.Message{})
}
