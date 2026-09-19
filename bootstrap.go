package main

import (
	"context"
	"errors"
	"fmt"
	"iChat/config"
	"iChat/database"
	"iChat/router"
	scheduledtasks "iChat/scheduledTasks"
	"iChat/service"
	"iChat/utils"

	"github.com/go-redis/redis/v8"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

// Factories perform no work until bootstrap calls them. A successful opener
// transfers ownership to bootstrap; a failing opener cleans up its own resources.
// startWorker must return a stop function that waits for all running jobs.
type startupDependencies struct {
	loadConfig  func() (config.Config, error)
	openMySQL   func(config.Config) (*gorm.DB, error)
	openRedis   func(context.Context, config.Config) (*redis.Client, error)
	startWorker func(scheduledtasks.MessageStore) (func(), error)
	serve       func(config.Config, *redis.Client, service.AccountUsers, service.ContactQueries, service.ContactWrites) error
}

func defaultStartupDependencies() startupDependencies {
	return startupDependencies{
		loadConfig: config.Load,
		openMySQL:  utils.OpenMySQL,
		openRedis:  utils.OpenRedis,
		startWorker: func(store scheduledtasks.MessageStore) (func(), error) {
			return startLegacyCron("@every 60s", func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						utils.Logger().WithField("panic", recovered).Error("sync messages panicked")
					}
				}()
				if err := scheduledtasks.SyncMsg(context.Background(), store); err != nil {
					utils.Logger().WithError(err).Error("sync messages failed")
				}
			})
		},
		serve: func(conf config.Config, rds *redis.Client, users service.AccountUsers, contacts service.ContactQueries, writes service.ContactWrites) error {
			tokens, err := utils.NewTokenService(conf, utils.NewRedisTokenBlacklist(rds))
			if err != nil {
				return err
			}
			return router.Router(conf, tokens, users, contacts, writes).Run()
		},
	}
}

// bootstrap currently owns the legacy, single-process dependency lifecycle.
// It must not run concurrently: handlers still read legacy global bindings.
// Failed dependencies prevent worker/HTTP startup until the later degraded-mode
// API and explicit service injection work replaces this transitional contract.
func bootstrap(deps startupDependencies) (err error) {
	if deps.loadConfig == nil || deps.openMySQL == nil || deps.openRedis == nil || deps.startWorker == nil || deps.serve == nil {
		return errors.New("bootstrap: missing startup dependency")
	}
	conf, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	db, err := deps.openMySQL(conf)
	if err != nil {
		return fmt.Errorf("initialize mysql: %w", err)
	}
	if db == nil || db.Config == nil {
		return errors.New("initialize mysql: opener returned no database")
	}
	pool, err := db.DB()
	if err != nil {
		return fmt.Errorf("get mysql connection pool: %w", err)
	}
	if pool == nil {
		return errors.New("initialize mysql: database has no connection pool")
	}
	defer func() {
		if closeErr := pool.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close mysql connection pool: %w", closeErr))
		}
	}()
	rds, err := deps.openRedis(context.Background(), conf)
	if err != nil {
		return fmt.Errorf("initialize redis: %w", err)
	}
	if rds == nil {
		return errors.New("initialize redis: opener returned no client")
	}
	defer func() {
		if closeErr := rds.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close redis client: %w", closeErr))
		}
	}()
	restore := bindLegacyDependencies(conf, db, rds)
	defer restore()
	stop, err := deps.startWorker(database.Mmanager)
	// Also stop a partially started worker if its factory supplies a stop handle.
	if stop != nil {
		defer stop()
	}
	if err != nil {
		return fmt.Errorf("start legacy worker: %w", err)
	}
	if stop == nil {
		return errors.New("start legacy worker: missing stop function")
	}
	contacts := service.ContactQueries{Friends: database.Rmanager, Groups: database.Gmanager}
	if err := deps.serve(conf, rds, database.Umanager, contacts, service.ContactWrites{Friends: database.Rmanager, Groups: database.Gmanager}); err != nil {
		return fmt.Errorf("serve http: %w", err)
	}
	return nil
}

// Keep the compatibility bridge in one place until handlers receive dependencies
// directly. Restore old bindings only after the worker has stopped, before closing
// clients, so failed/repeated startup does not leave closed clients in globals.
func bindLegacyDependencies(conf config.Config, db *gorm.DB, rds *redis.Client) func() {
	oldConf, oldDB, oldRDS := config.Conf, utils.DB, utils.RDS
	oldUser, oldRelation := database.Umanager, database.Rmanager
	oldGroup, oldMessage := database.Gmanager, database.Mmanager
	config.Conf, utils.DB, utils.RDS = conf, db, rds
	database.Umanager = database.NewUserManager(db)
	database.Rmanager = database.NewRelationManager(db)
	database.Gmanager = database.NewGroupManager(db)
	database.Mmanager = database.NewMessageManager(db, rds)
	return func() {
		config.Conf, utils.DB, utils.RDS = oldConf, oldDB, oldRDS
		database.Umanager, database.Rmanager = oldUser, oldRelation
		database.Gmanager, database.Mmanager = oldGroup, oldMessage
	}
}

func startLegacyCron(schedule string, job func()) (func(), error) {
	if job == nil {
		return nil, errors.New("legacy cron requires a job")
	}
	c := cron.New()
	if _, err := c.AddFunc(schedule, job); err != nil {
		return nil, err
	}
	c.Start()
	return func() { <-c.Stop().Done() }, nil
}
