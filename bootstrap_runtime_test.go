package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"iChat/config"
	"iChat/router"
	scheduledtasks "iChat/scheduledTasks"
	"iChat/service"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

func TestBootstrapRuntimeGuardBeforeExternalDependencies(t *testing.T) {
	for _, tc := range []struct {
		mode         config.RuntimeMode
		legacyWorker bool
		want         error
	}{
		{config.ModeAPI, false, ErrRuntimeNotAssembled}, {config.ModeWorker, false, ErrRuntimeNotAssembled},
		{config.ModeAll, false, ErrRuntimeNotAssembled}, {config.RuntimeMode("SECRET_TOKEN"), false, config.ErrRuntimeConfig},
		{config.ModeAPI, true, config.ErrRuntimeConfig}, {config.ModeWorker, true, config.ErrRuntimeConfig}, {config.ModeAll, true, config.ErrRuntimeConfig},
	} {
		t.Run(string(tc.mode)+"_legacy_"+map[bool]string{true: "true", false: "false"}[tc.legacyWorker], func(t *testing.T) {
			var conf config.Config
			conf.RUNTIME.Mode, conf.LEGACY_WORKER.Enabled = tc.mode, tc.legacyWorker
			conf.MYSQL.Password, conf.RABBITMQ.URL = "SECRET_TOKEN", "amqp://user:SECRET_TOKEN@must-not-resolve.invalid/vhost"
			before := legacyState()
			var calls []string
			unexpected := func(name string) { calls = append(calls, name); t.Errorf("runtime guard called %s", name) }
			deps := startupDependencies{
				loadConfig: func() (config.Config, error) { calls = append(calls, "config"); return conf, nil },
				openMySQL: func(config.Config) (*gorm.DB, error) {
					unexpected("mysql")
					return nil, errors.New("forbidden factory")
				},
				openRedis: func(context.Context, config.Config) (*redis.Client, error) {
					unexpected("redis")
					return nil, errors.New("forbidden factory")
				},
				startWorker: func(scheduledtasks.MessageStore, *redis.Client) (func(), error) {
					unexpected("legacy worker")
					return nil, errors.New("forbidden factory")
				},
				serve: func(config.Config, *redis.Client, service.AccountUsers, service.ContactQueries, service.ContactWrites, service.MessageStore, service.MessageSubscriptions, router.StorageHealth) error {
					unexpected("http")
					return nil
				},
			}
			err := bootstrap(deps)
			if !errors.Is(err, tc.want) {
				t.Fatalf("runtime startup error=%v", err)
			}
			if strings.Contains(err.Error(), "SECRET_TOKEN") {
				t.Fatal("bootstrap error leaked config input")
			}
			if !reflect.DeepEqual(calls, []string{"config"}) || !reflect.DeepEqual(legacyState(), before) {
				t.Fatal("guard had external or global side effects")
			}
		})
	}
}
