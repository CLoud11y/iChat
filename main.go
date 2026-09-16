package main

import (
	"context"
	"fmt"
	"iChat/config"
	"iChat/database"
	"iChat/router"
	scheduledtasks "iChat/scheduledTasks"
	"iChat/utils"
	"os"

	"github.com/robfig/cron/v3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	conf, err := config.Load()
	if err != nil {
		return err
	}
	db, err := utils.OpenMySQL(conf)
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("get mysql connection pool: %w", err)
	}
	defer sqlDB.Close()
	rds, err := utils.OpenRedis(context.Background(), conf)
	if err != nil {
		return err
	}
	defer rds.Close()
	config.Conf = conf
	utils.DB, utils.RDS = db, rds
	database.Umanager = database.NewUserManager(db)
	database.Rmanager = database.NewRelationManager(db)
	database.Gmanager = database.NewGroupManager(db)
	database.Mmanager = database.NewMessageManager(db, rds)
	// 启动定时任务
	ctx, cancel := context.WithCancel(context.Background())
	cronDone := make(chan struct{})
	go func() {
		defer close(cronDone)
		startCron(ctx)
	}()
	defer func() {
		cancel()
		<-cronDone
	}()

	r := router.Router()
	return r.Run() // listen and serve on 0.0.0.0:8080
}

func startCron(ctx context.Context) {
	c := cron.New()
	c.AddFunc("@every 60s", scheduledtasks.SyncMsg)
	c.Start()
	<-ctx.Done()
	<-c.Stop().Done()
}
