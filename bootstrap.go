package main

import (
	"context"
	"errors"
	"fmt"
	"iChat/config"
	"iChat/database"
	"iChat/health"
	"iChat/router"
	scheduledtasks "iChat/scheduledTasks"
	"iChat/service"
	"iChat/utils"
	"time"

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
	startWorker func(scheduledtasks.MessageStore, *redis.Client) (func(), error)
	serve       func(config.Config, *redis.Client, service.AccountUsers, service.ContactQueries, service.ContactWrites, service.MessageStore, service.MessageSubscriptions, router.StorageHealth) error
}

func defaultStartupDependencies() startupDependencies {
	return startupDependencies{
		loadConfig: config.Load,
		openMySQL:  utils.NewMySQL,
		openRedis:  utils.OpenRedis,
		startWorker: func(store scheduledtasks.MessageStore, rds *redis.Client) (func(), error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			owner, err := database.AcquireLegacyWorker(ctx, rds)
			cancel()
			if err != nil {
				return nil, err
			}
			release := func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := owner.Release(ctx); err != nil {
					utils.Logger().WithError(err).Error("release legacy worker ownership failed; operator recovery may be required")
				}
			}
			store = scheduledtasks.OwnedMessages{MessageStore: store, Owner: owner}
			stop, err := startLegacyCron("@every 60s", func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						utils.Logger().WithField("panic", recovered).Error("sync messages panicked")
					}
				}()
				if err := scheduledtasks.SyncMsg(context.Background(), store); err != nil {
					utils.Logger().WithError(err).Error("sync messages failed")
				}
			})
			if err != nil {
				release()
				return nil, err
			}
			return func() { stop(); release() }, nil
		},
		serve: func(conf config.Config, rds *redis.Client, users service.AccountUsers, contacts service.ContactQueries, writes service.ContactWrites, messages service.MessageStore, subscriptions service.MessageSubscriptions, storage router.StorageHealth) error {
			tokens, err := utils.NewTokenService(conf, utils.NewRedisTokenBlacklist(rds))
			if err != nil {
				return err
			}
			return router.Router(conf, tokens, users, contacts, writes, messages, subscriptions, storage).Run()
		},
	}
}

// bootstrap currently owns the legacy, single-process dependency lifecycle.
// It must not run concurrently because it owns legacy global bindings.
// MySQL construction is lazy: an offline server does not prevent HTTP startup.
// Invalid handles/configuration and Redis startup failures remain fatal.
func bootstrap(deps startupDependencies) (err error) {
	if deps.loadConfig == nil || deps.openMySQL == nil || deps.openRedis == nil || deps.serve == nil {
		return errors.New("bootstrap: missing startup dependency")
	}
	conf, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	if conf.LEGACY_WORKER.Enabled && deps.startWorker == nil {
		return errors.New("bootstrap: missing startup dependency for enabled legacy worker")
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
	if conf.LEGACY_WORKER.Enabled {
		stop, err := deps.startWorker(database.Mmanager, rds)
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
	}
	storage := health.StartStorage(pool.PingContext, 5*time.Second, 2*time.Second)
	defer storage.Stop()
	contacts := service.ContactQueries{Friends: database.Rmanager, Groups: database.Gmanager}
	if err := deps.serve(conf, rds, database.Umanager, contacts, service.ContactWrites{Friends: database.Rmanager, Groups: database.Gmanager}, database.Mmanager, database.Mmanager, storage); err != nil {
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
	// Recovery must be INSIDE the skip wrapper: cron v3 releases its token
	// after Run returns, so an escaping panic would otherwise strand the token.
	c := cron.New(cron.WithChain(
		cron.SkipIfStillRunning(cron.DefaultLogger),
		cron.Recover(cron.DefaultLogger),
	))
	if _, err := c.AddFunc(schedule, job); err != nil {
		return nil, err
	}
	c.Start()
	return func() { <-c.Stop().Done() }, nil
}
