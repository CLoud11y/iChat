package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"iChat/config"
	"iChat/database"
	"iChat/service"
	"iChat/utils"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

// No socket is opened: this pool implements only the lifecycle operations used
// by bootstrap, and rejects SQL so an accidental startup migration fails tests.
type lifecycleConnector struct{ close func() error }
type lifecycleDriver struct{}
type lifecycleConnection struct{ close func() error }

func (c lifecycleConnector) Connect(context.Context) (driver.Conn, error) {
	return lifecycleConnection{close: c.close}, nil
}
func (lifecycleConnector) Driver() driver.Driver { return lifecycleDriver{} }
func (lifecycleDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("unexpected driver Open")
}
func (c lifecycleConnection) Close() error { return c.close() }
func (lifecycleConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("SQL is not allowed during bootstrap")
}
func (lifecycleConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not allowed during bootstrap")
}

func legacyState() []any {
	return []any{config.Conf, utils.DB, utils.RDS, database.Umanager, database.Rmanager, database.Gmanager, database.Mmanager}
}

func TestBootstrapLifecycle(t *testing.T) {
	// Preserve a nonempty preexisting application to catch implementations that
	// merely clear globals rather than restoring their original identities.
	var priorConf config.Config
	priorConf.App.Name = "previous-application"
	priorRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	defer priorRedis.Close()
	defer bindLegacyDependencies(priorConf, &gorm.DB{}, priorRedis)()
	failure := errors.New("injected dependency failure")
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"config", []string{"config"}},
		{"mysql", []string{"config", "mysql"}},
		{"nil_mysql", []string{"config", "mysql"}},
		{"uninitialized_mysql", []string{"config", "mysql"}},
		{"invalid_pool", []string{"config", "mysql"}},
		{"redis", []string{"config", "mysql", "redis", "close_mysql"}},
		{"nil_redis", []string{"config", "mysql", "redis", "close_mysql"}},
		{"worker", []string{"config", "mysql", "redis", "worker", "close_mysql"}},
		{"partial_worker", []string{"config", "mysql", "redis", "worker", "stop_worker", "close_mysql"}},
		{"missing_stop", []string{"config", "mysql", "redis", "worker", "close_mysql"}},
		{"http", []string{"config", "mysql", "redis", "worker", "http", "stop_worker", "close_mysql"}},
		{"success", []string{"config", "mysql", "redis", "worker", "http", "stop_worker", "close_mysql"}},
		{"close", []string{"config", "mysql", "redis", "worker", "http", "stop_worker", "close_mysql"}},
		{"http_and_close", []string{"config", "mysql", "redis", "worker", "http", "stop_worker", "close_mysql"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := legacyState()
			var events []string
			var conf config.Config
			conf.App.Name = "injected-bootstrap"
			conf.MYSQL.Host = "must-not-connect"
			var db *gorm.DB
			var rds *redis.Client
			closeFailure := errors.New("injected pool close failure")
			checkBound := func() {
				t.Helper()
				if utils.DB != db || utils.RDS != rds || config.Conf != conf || database.Umanager == nil || database.Rmanager == nil || database.Gmanager == nil || database.Mmanager == nil {
					t.Error("worker/HTTP must see fully initialized dependencies")
				}
				if err := rds.Ping(context.Background()).Err(); errors.Is(err, redis.ErrClosed) {
					t.Error("Redis was closed before worker stopped")
				}
				pool, err := db.DB()
				if err != nil || pool.Ping() != nil {
					t.Error("MySQL was closed before worker stopped")
				}
			}
			deps := startupDependencies{
				loadConfig: func() (config.Config, error) {
					events = append(events, "config")
					if tc.name == "config" {
						return config.Config{}, failure
					}
					return conf, nil
				},
				openMySQL: func(got config.Config) (*gorm.DB, error) {
					events = append(events, "mysql")
					if got != conf {
						t.Fatal("MySQL factory did not receive loaded config")
					}
					switch tc.name {
					case "mysql":
						return nil, failure
					case "nil_mysql":
						return nil, nil
					case "uninitialized_mysql":
						return &gorm.DB{}, nil
					case "invalid_pool":
						return &gorm.DB{Config: &gorm.Config{}}, nil
					}
					pool := sql.OpenDB(lifecycleConnector{close: func() error {
						events = append(events, "close_mysql")
						if !reflect.DeepEqual(legacyState(), original) {
							t.Error("global dependencies not restored before closing MySQL")
						}
						if rds != nil && !errors.Is(rds.Ping(context.Background()).Err(), redis.ErrClosed) {
							t.Error("Redis must close before MySQL")
						}
						if tc.name == "close" || tc.name == "http_and_close" {
							return closeFailure
						}
						return nil
					}})
					t.Cleanup(func() { _ = pool.Close() })
					if err := pool.Ping(); err != nil {
						t.Fatal(err)
					}
					db = &gorm.DB{Config: &gorm.Config{ConnPool: pool}}
					return db, nil
				},
				openRedis: func(_ context.Context, got config.Config) (*redis.Client, error) {
					events = append(events, "redis")
					if got != conf || !reflect.DeepEqual(legacyState(), original) {
						t.Fatal("configuration mismatch or globals changed before all dependencies were ready")
					}
					if tc.name == "redis" {
						return nil, failure
					}
					if tc.name == "nil_redis" {
						return nil, nil
					}
					rds = redis.NewClient(&redis.Options{
						MaxRetries: -1,
						Dialer: func(context.Context, string, string) (net.Conn, error) {
							return nil, errors.New("test Redis dialer: no network")
						},
					})
					t.Cleanup(func() { _ = rds.Close() })
					return rds, nil
				},
				startWorker: func() (func(), error) {
					events = append(events, "worker")
					checkBound()
					if tc.name == "worker" {
						return nil, failure
					}
					if tc.name == "missing_stop" {
						return nil, nil
					}
					stop := func() {
						events = append(events, "stop_worker")
						checkBound()
					}
					if tc.name == "partial_worker" {
						return stop, failure
					}
					return stop, nil
				},
				serve: func(got config.Config, client *redis.Client, users service.AccountUsers) error {
					if got != conf || client != rds || users != database.Umanager {
						t.Fatal("HTTP factory did not receive supplied dependencies")
					}
					events = append(events, "http")
					checkBound()
					if tc.name == "http" || tc.name == "http_and_close" {
						return failure
					}
					return nil
				},
			}
			err := bootstrap(deps)
			if (err == nil) != (tc.name == "success") {
				t.Fatalf("bootstrap error=%v", err)
			}
			switch tc.name {
			case "config", "mysql", "redis", "worker", "partial_worker", "http", "http_and_close":
				if !errors.Is(err, failure) {
					t.Errorf("original failure lost: %v", err)
				}
			}
			if (tc.name == "close" || tc.name == "http_and_close") && !errors.Is(err, closeFailure) {
				t.Errorf("pool close failure lost: %v", err)
			}
			if !reflect.DeepEqual(events, tc.want) {
				t.Errorf("lifecycle=%v, want %v", events, tc.want)
			}
			if !reflect.DeepEqual(legacyState(), original) {
				t.Error("bootstrap left stale global dependencies")
			}
		})
	}
}

func TestBootstrapMissingFactory(t *testing.T) {
	for _, name := range []string{"config", "mysql", "redis", "worker", "http"} {
		t.Run(name, func(t *testing.T) {
			deps := defaultStartupDependencies()
			deps.loadConfig = func() (config.Config, error) {
				t.Fatal("missing dependencies must be rejected before loading config")
				return config.Config{}, nil
			}
			switch name {
			case "config":
				deps.loadConfig = nil
			case "mysql":
				deps.openMySQL = nil
			case "redis":
				deps.openRedis = nil
			case "worker":
				deps.startWorker = nil
			case "http":
				deps.serve = nil
			}
			if err := bootstrap(deps); err == nil || !strings.Contains(err.Error(), "missing startup dependency") {
				t.Fatalf("bootstrap error=%v", err)
			}
		})
	}
}

func TestLegacyCronWaitsForRunningJob(t *testing.T) {
	started, release, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	stop, err := startLegacyCron("@every 1s", func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unblock(); stop() })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("cron did not start a job")
	}
	go func() { stop(); close(completed) }()
	select {
	case <-completed:
		t.Fatal("stop returned before running job completed")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return after job completed")
	}
}

func TestLegacyCronInvalidConfiguration(t *testing.T) {
	if stop, err := startLegacyCron("invalid schedule", func() {}); err == nil || stop != nil {
		t.Fatal("invalid schedule started a worker")
	}
	if stop, err := startLegacyCron("@every 60s", nil); err == nil || stop != nil {
		t.Fatal("nil job started a worker")
	}
}
