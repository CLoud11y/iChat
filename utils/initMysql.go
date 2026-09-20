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
	return openMySQL(conf, false)
}

// NewMySQL creates a pool without contacting the server. API startup can bind
// this stable handle while MySQL is offline; database/sql retries connections on
// subsequent operations. This does not assert storage readiness or migrate.
// The application targets MySQL 8; version discovery is deliberately skipped.
func NewMySQL(conf config.Config) (*gorm.DB, error) {
	return openMySQL(conf, true)
}

func openMySQL(conf config.Config, lazy bool) (*gorm.DB, error) {
	// 初始化数据库连接
	mysqlConf := &conf.MYSQL
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		mysqlConf.User,
		mysqlConf.Password,
		mysqlConf.Host,
		mysqlConf.Port,
		mysqlConf.DbName,
	)
	if lazy {
		// Bound connection and socket I/O when callers have no deadline.
		dsn += "&timeout=3s&readTimeout=5s&writeTimeout=5s"
	}
	MysqlConf := mysql.New(mysql.Config{DSN: dsn, SkipInitializeWithVersion: lazy})
	GormConf := &gorm.Config{DisableAutomaticPing: lazy}

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
	return db.AutoMigrate(&models.User{}, &models.Relation{}, &models.Group{}, &models.Message{}, &models.LegacyMessageQuarantine{})
}
