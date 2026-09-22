package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"iChat/config"
	"iChat/database"
	"iChat/router"
	scheduledtasks "iChat/scheduledTasks"
	"iChat/service"
	"iChat/utils"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

func TestBootstrapMySQLOffline(t *testing.T) {
	// A local endpoint rejects every MySQL handshake; no external DB is used.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	defer func() { _ = listener.Close(); <-done }()
	var conf config.Config
	conf.LEGACY_WORKER.Enabled = true
	conf.MYSQL.Host, conf.MYSQL.Port, err = net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conf.MYSQL.User, conf.MYSQL.DbName = "offline", "offline"
	original := legacyState()
	deps := defaultStartupDependencies()
	deps.loadConfig = func() (config.Config, error) { return conf, nil }
	var pool *sql.DB
	opener := deps.openMySQL
	deps.openMySQL = func(c config.Config) (*gorm.DB, error) {
		db, err := opener(c)
		if err != nil {
			return nil, err
		}
		pool, err = db.DB()
		if err != nil {
			return nil, err
		}
		if pool.Stats().OpenConnections != 0 {
			t.Error("constructor connected to MySQL")
		}
		return db, nil
	}
	deps.openRedis = func(context.Context, config.Config) (*redis.Client, error) {
		return redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"}), nil
	}
	started, stopped, served := false, false, false
	deps.startWorker = func(scheduledtasks.MessageStore) (func(), error) {
		started = true
		return func() { stopped = true }, nil
	}
	deps.serve = func(_ config.Config, _ *redis.Client, users service.AccountUsers, _ service.ContactQueries, _ service.ContactWrites, _ service.MessageStore, _ service.MessageSubscriptions, _ router.StorageHealth) error {
		served = true
		r := gin.New()
		r.POST("/login", service.LoginUser(users, nil))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"account":"offline","password":"password"}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		r.ServeHTTP(response, req)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "DEPENDENCY_UNAVAILABLE") {
			t.Errorf("offline login = %d %s", response.Code, response.Body.String())
		}
		return nil
	}
	if err := bootstrap(deps); err != nil {
		t.Fatal(err)
	}
	if !started || !stopped || !served {
		t.Fatal("offline startup did not complete worker/HTTP lifecycle")
	}
	if !reflect.DeepEqual(original, legacyState()) {
		t.Fatal("globals not restored")
	}
	if err := pool.Ping(); err == nil || !strings.Contains(err.Error(), "database is closed") {
		t.Fatalf("pool not closed: %v", err)
	}
}

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
		{"disabled", []string{"config", "mysql", "redis", "http", "close_mysql"}},
		{"disabled_nil_worker", []string{"config", "mysql", "redis", "http", "close_mysql"}},
		{"close", []string{"config", "mysql", "redis", "worker", "http", "stop_worker", "close_mysql"}},
		{"http_and_close", []string{"config", "mysql", "redis", "worker", "http", "stop_worker", "close_mysql"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := legacyState()
			var events []string
			var conf config.Config
			conf.LEGACY_WORKER.Enabled = tc.name != "disabled" && tc.name != "disabled_nil_worker"
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
					// The event log counts physical connection closes. Keep one
					// connection while the health probe and assertions share the pool.
					pool.SetMaxOpenConns(1)
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
				startWorker: func(store scheduledtasks.MessageStore) (func(), error) {
					if store != database.Mmanager {
						t.Fatal("worker did not receive supplied message store")
					}
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
				serve: func(got config.Config, client *redis.Client, users service.AccountUsers, contacts service.ContactQueries, writes service.ContactWrites, messages service.MessageStore, subscriptions service.MessageSubscriptions, storage router.StorageHealth) error {
					if storage == nil {
						t.Fatal("HTTP factory did not receive storage health")
					}
					if got != conf || client != rds || users != database.Umanager || contacts.Friends != database.Rmanager || contacts.Groups != database.Gmanager || writes.Friends != database.Rmanager || writes.Groups != database.Gmanager || messages != database.Mmanager || subscriptions != database.Mmanager {
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
			if tc.name == "disabled_nil_worker" {
				deps.startWorker = nil
			}
			err := bootstrap(deps)
			if (err == nil) != (tc.name == "success" || tc.name == "disabled" || tc.name == "disabled_nil_worker") {
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
				if name == "worker" {
					var conf config.Config
					conf.LEGACY_WORKER.Enabled = true
					return conf, nil
				}
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

func TestLegacyCronSkipsOverlappingRuns(t *testing.T) {
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	stop, err := startLegacyCron("@every 1s", func() { started <- struct{}{}; <-release })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unblock(); stop() })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("job did not start")
	}
	select {
	case <-started:
		t.Fatal("a second job started while the first was still running")
	case <-time.After(2200 * time.Millisecond):
	}
	unblock()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("job did not resume after the previous run completed")
	}
}

func TestLegacyCronResumesAfterPanic(t *testing.T) {
	var first sync.Once
	resumed := make(chan struct{}, 1)
	stop, err := startLegacyCron("@every 1s", func() {
		first.Do(func() { panic("injected legacy job panic") })
		select {
		case resumed <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	select {
	case <-resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("panic stranded the non-reentrancy token")
	}
}
