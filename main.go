package main

import (
	"context"
	"iChat/router"
	scheduledtasks "iChat/scheduledTasks"

	"github.com/robfig/cron/v3"
)

func main() {
	// 启动定时任务
	ctx, cancel := context.WithCancel(context.Background())
	go startCron(ctx)
	defer cancel()

	r := router.Router()
	r.Run() // listen and serve on 0.0.0.0:8080 (for windows "localhost:8080")
}

func startCron(ctx context.Context) {
	c := cron.New()
	c.AddFunc("@every 60s", scheduledtasks.SyncMsg)
	c.Start()
	<-ctx.Done()
	c.Stop()
}
