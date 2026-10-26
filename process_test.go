package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"iChat/application"
	"iChat/config"
	"iChat/router"
	scheduledtasks "iChat/scheduledTasks"
	"iChat/service"
	"iChat/workers"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

func processTestConfig(mode config.RuntimeMode) config.Config {
	return config.Config{
		RUNTIME: config.RuntimeConfig{Mode: mode},
		RABBITMQ: config.RabbitMQConfig{
			URL: "amqp://app:SECRET_TOKEN@127.0.0.1:9/%2Fichat", VHost: "/ichat",
			ManagementURL: "http://127.0.0.1:9", ManagementUsername: "admin", ManagementPassword: "SECRET_TOKEN",
			PublisherSlots: 1, ConsumerWorkers: 2, ConsumerPrefetch: 3,
			PublishTimeout: time.Second, ConsumerOperationTimeout: time.Second,
			ConnectTimeout: time.Second, WriteTimeout: time.Second, CloseTimeout: time.Second, Heartbeat: time.Second,
			RetryMin: time.Millisecond, RetryMax: 2 * time.Millisecond,
		},
	}
}

func processTestFactories() application.RuntimeFactories {
	factory := func(context.Context, application.RoleDependencies) (application.RuntimeService, error) {
		panic("process entry must not construct services itself")
	}
	return application.RuntimeFactories{API: factory, Persistence: factory, Quarantine: factory, Outbox: factory}
}

func TestRunProcessDefaultOutboxFactoryIsLazyAndExplicitWins(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		deps := defaultProcessDependencies()
		conf := apiTestConfig()
		conf.RUNTIME.Mode = config.ModeWorker
		deps.legacy.loadConfig = func() (config.Config, error) { return conf, nil }
		deps.factories = processTestFactories()
		if !explicit {
			deps.factories.Outbox = nil
		}
		calls := 0
		original := deps.outbox
		deps.outbox = func(c config.Config) (application.RuntimeFactory, error) { calls++; return original(c) }
		deps.notifyContext = func(c context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) { return c, func() {} }
		deps.runRuntime = func(c context.Context, _ config.Config, f application.RuntimeFactories, _ application.RuntimeOptions) error {
			if calls != 0 {
				t.Fatal("production resource factory ran before runtime construction")
			}
			if explicit {
				return nil
			}
			s, err := f.Outbox(c, application.RoleDependencies{})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := s.(*workers.OutboxService); !ok {
				t.Fatal("default Outbox is not production service")
			}
			if s.(application.RuntimeOutboxReadiness).OutboxReady() {
				t.Fatal("idle factory became ready")
			}
			return s.Close(c)
		}
		if err := runProcess(deps); err != nil {
			t.Fatal(err)
		}
		want := 1
		if explicit {
			want = 0
		}
		if calls != want {
			t.Fatal("explicit role was replaced", calls)
		}
	}
}

func TestRunProcessGuardBeforeDependenciesAndSignals(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode config.RuntimeMode
		old  bool
		want error
	}{
		{"api", config.ModeAPI, false, application.ErrRuntimeNotAssembled},
		{"worker", config.ModeWorker, false, application.ErrRuntimeNotAssembled},
		{"all", config.ModeAll, false, application.ErrRuntimeNotAssembled},
		{"all_api_only", config.ModeAll, false, application.ErrRuntimeNotAssembled},
		{"worker_api_only", config.ModeWorker, false, application.ErrRuntimeNotAssembled},
		{"invalid", config.RuntimeMode("SECRET_TOKEN"), false, config.ErrRuntimeConfig},
		{"api_conflict", config.ModeAPI, true, config.ErrRuntimeConfig},
		{"worker_conflict", config.ModeWorker, true, config.ErrRuntimeConfig},
		{"all_conflict", config.ModeAll, true, config.ErrRuntimeConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := legacyState()
			deps := defaultProcessDependencies()
			deps.api = nil // This matrix deliberately supplies no production defaults.
			deps.outbox = nil
			if strings.HasSuffix(tc.name, "_api_only") {
				deps.factories.API = processTestFactories().API
			}
			var calls []string
			deps.legacy = startupDependencies{loadConfig: func() (config.Config, error) {
				calls = append(calls, "config")
				conf := processTestConfig(tc.mode)
				conf.LEGACY_WORKER.Enabled = tc.old
				return conf, nil
			}}
			deps.notifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
				t.Fatal("guard registered process signals")
				return nil, nil
			}
			deps.runRuntime = func(context.Context, config.Config, application.RuntimeFactories, application.RuntimeOptions) error {
				t.Fatal("guard entered runtime")
				return nil
			}
			err := runProcess(deps)
			if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "SECRET_TOKEN") {
				t.Fatalf("guard error=%v", err)
			}
			if tc.want == application.ErrRuntimeNotAssembled {
				var missing *application.RuntimeAssemblyError
				if !errors.As(err, &missing) || missing.Mode() != tc.mode {
					t.Fatal("missing structured assembly error", err)
				}
				want := []application.RuntimeRole{application.RoleAPI}
				if tc.mode == config.ModeWorker {
					want = []application.RuntimeRole{application.RolePersistence, application.RoleQuarantine, application.RoleOutbox}
				} else if tc.mode == config.ModeAll {
					want = append(want, application.RolePersistence, application.RoleQuarantine, application.RoleOutbox)
				}
				if tc.mode == config.ModeAll && deps.factories.API != nil {
					want = want[1:]
				}
				if !reflect.DeepEqual(missing.MissingRoles(), want) {
					t.Fatal("incomplete role diagnostic", missing.MissingRoles())
				}
			}
			if !reflect.DeepEqual(calls, []string{"config"}) || !reflect.DeepEqual(legacyState(), before) {
				t.Fatal("guard changed globals or called dependencies")
			}
		})
	}
}

func TestRunProcessModernDispatch(t *testing.T) {
	for _, mode := range []config.RuntimeMode{config.ModeAPI, config.ModeWorker, config.ModeAll} {
		t.Run(string(mode), func(t *testing.T) {
			before := legacyState()
			conf := processTestConfig(mode)
			deps := defaultProcessDependencies()
			// Modern entry does not require any legacy opener/server/worker.
			var events []string
			deps.legacy = startupDependencies{loadConfig: func() (config.Config, error) {
				events = append(events, "config")
				return conf, nil
			}}
			deps.factories = processTestFactories()
			if mode == config.ModeAPI {
				deps.factories.Persistence, deps.factories.Quarantine, deps.factories.Outbox = nil, nil, nil
			} else if mode == config.ModeWorker {
				deps.factories.API = nil
			}
			deps.options.DrainTimeout = 31 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deps.notifyContext = func(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
				events = append(events, "signals")
				if parent.Err() != nil || parent.Done() != nil || !reflect.DeepEqual(signals, []os.Signal{os.Interrupt, syscall.SIGTERM}) {
					t.Fatal("incorrect process signal subscription", signals)
				}
				return ctx, func() { events = append(events, "unregister"); cancel() }
			}
			failure := errors.New("runtime failure")
			deps.runRuntime = func(got context.Context, c config.Config, f application.RuntimeFactories, o application.RuntimeOptions) error {
				events = append(events, "runtime")
				if got != ctx || got.Err() != nil || c != conf || o != deps.options {
					t.Fatal("entry changed runtime inputs")
				}
				for _, pair := range [][2]application.RuntimeFactory{{f.API, deps.factories.API}, {f.Persistence, deps.factories.Persistence}, {f.Quarantine, deps.factories.Quarantine}, {f.Outbox, deps.factories.Outbox}} {
					if reflect.ValueOf(pair[0]).Pointer() != reflect.ValueOf(pair[1]).Pointer() {
						t.Fatal("entry replaced a business factory")
					}
				}
				return failure
			}
			if err := runProcess(deps); err != failure {
				t.Fatal("runtime error was replaced", err)
			}
			if ctx.Err() != context.Canceled || !reflect.DeepEqual(events, []string{"config", "signals", "runtime", "unregister"}) || !reflect.DeepEqual(before, legacyState()) {
				t.Fatal("incorrect dispatch/unregister lifecycle", events)
			}
		})
	}
}

func TestRunProcessDefaultAPIAssemblyAndPreflight(t *testing.T) {
	for _, name := range []string{"api", "worker", "all", "bad_jwt", "bad_address", "bad_redis", "bad_mysql"} {
		t.Run(name, func(t *testing.T) {
			h := newAPIHarness()
			deps := defaultProcessDependencies()
			deps.api = &apiAssembly{options: func(config.Config) apiOptions { return h.o }, dependencies: h.d}
			switch name {
			case "worker":
				h.c.RUNTIME.Mode = config.ModeWorker
			case "all":
				h.c.RUNTIME.Mode = config.ModeAll
			case "bad_jwt":
				h.c.JWT.Key = ""
			case "bad_address":
				h.o.Address = "invalid"
			case "bad_redis":
				h.c.REDIS.Addr = "invalid"
			case "bad_mysql":
				h.c.MYSQL.DbName = ""
			}
			deps.legacy.loadConfig = func() (config.Config, error) { return h.c, nil }
			registered, executed := false, false
			deps.notifyContext = func(ctx context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
				registered = true
				return ctx, func() {}
			}
			deps.runRuntime = func(ctx context.Context, c config.Config, f application.RuntimeFactories, _ application.RuntimeOptions) error {
				executed = true
				if f.API == nil || f.Persistence != nil || f.Quarantine != nil || f.Outbox != nil {
					t.Fatal("incorrect production role selection")
				}
				service, err := f.API(ctx, application.RoleDependencies{Publisher: h.publisher, Health: apiTestHealth{}})
				if err != nil {
					return err
				}
				if _, ok := service.(*apiService); !ok {
					t.Fatal("default API is not the production service")
				}
				return service.Close(context.Background())
			}
			err := runProcess(deps)
			if name == "api" {
				if err != nil || !registered || !executed {
					t.Fatal("default API not dispatched", err)
				}
			} else {
				if err == nil || registered || executed {
					t.Fatal("invalid/incomplete configuration reached runtime", err)
				}
				if name == "all" || name == "worker" {
					var missing *application.RuntimeAssemblyError
					if !errors.As(err, &missing) || !reflect.DeepEqual(missing.MissingRoles(), []application.RuntimeRole{application.RolePersistence, application.RoleQuarantine}) {
						t.Fatal("missing background roles not reported", err)
					}
				} else if !errors.Is(err, errAPIInput) {
					t.Fatal("missing API validation error", err)
				}
			}
			if h.sql.closed.Load() != 0 || h.redis.closed.Load() != 0 || h.redis.gets.Load() != 0 {
				t.Fatal("preflight/idle constructor opened resources")
			}
		})
	}
}

func TestRunProcessSignalCancellationWaitsForRuntimeCleanup(t *testing.T) {
	for _, shutdownFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "cleanup_error"}[shutdownFailure], func(t *testing.T) {
			deps := defaultProcessDependencies()
			deps.legacy = startupDependencies{loadConfig: func() (config.Config, error) { return processTestConfig(config.ModeAll), nil }}
			deps.factories = processTestFactories()
			ctx, signalCancel := context.WithCancel(context.Background())
			defer signalCancel()
			unregistered := make(chan struct{})
			deps.notifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
				return ctx, func() { close(unregistered); signalCancel() }
			}
			started, draining, allowFinish := make(chan struct{}), make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(allowFinish) })
			defer release()
			failure := errors.Join(application.ErrRuntimeShutdown, context.Canceled)
			deps.runRuntime = func(got context.Context, _ config.Config, _ application.RuntimeFactories, _ application.RuntimeOptions) error {
				close(started)
				<-got.Done()
				close(draining)
				<-allowFinish
				if shutdownFailure {
					return failure
				}
				return nil
			}
			result := make(chan error, 1)
			go func() { result <- runProcess(deps) }()
			await := func(ch <-chan struct{}) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(2 * time.Second):
					t.Fatal("process did not reach expected lifecycle stage")
				}
			}
			await(started)
			signalCancel()
			await(draining)
			signalCancel() // repeated stop requests do not interrupt cleanup
			select {
			case <-unregistered:
				t.Fatal("entry unregistered signals before runtime cleanup completed")
			case <-result:
				t.Fatal("entry abandoned runtime cleanup")
			default:
			}
			release()
			select {
			case err := <-result:
				if shutdownFailure && (err != failure || !errors.Is(err, application.ErrRuntimeShutdown) || !errors.Is(err, context.Canceled)) {
					t.Fatal("entry suppressed joined shutdown error", err)
				}
				if !shutdownFailure && err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("entry did not join runtime")
			}
			await(unregistered)
		})
	}
}

func TestRunProcessRealExecutorPrecancelled(t *testing.T) {
	deps := defaultProcessDependencies() // retain the production RunRuntime
	deps.legacy = startupDependencies{loadConfig: func() (config.Config, error) { return processTestConfig(config.ModeAll), nil }}
	deps.factories = processTestFactories()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stops := 0
	deps.notifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
		return ctx, func() { stops++ }
	}
	// The real executor rejects cancellation before MQ preparation or any role
	// factory. Startup cancellation is an error, not a successful drain.
	if err := runProcess(deps); !errors.Is(err, context.Canceled) || stops != 1 {
		t.Fatalf("pre-cancelled real executor error=%v, unregister=%d", err, stops)
	}
}

func TestRunProcessInvalidInputs(t *testing.T) {
	failure := errors.New("configuration failure")
	for _, tc := range []string{"loader", "load_failure", "runner", "notifier", "mq", "context", "stop"} {
		t.Run(tc, func(t *testing.T) {
			deps := defaultProcessDependencies()
			deps.factories = processTestFactories()
			conf := processTestConfig(config.ModeAPI)
			deps.legacy = startupDependencies{loadConfig: func() (config.Config, error) { return conf, nil }}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stops := 0
			deps.notifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
				if tc == "context" {
					return nil, func() { stops++ }
				}
				if tc == "stop" {
					return ctx, nil
				}
				t.Fatal("invalid input registered signals")
				return nil, nil
			}
			deps.runRuntime = func(context.Context, config.Config, application.RuntimeFactories, application.RuntimeOptions) error {
				t.Fatal("invalid input entered runtime")
				return nil
			}
			switch tc {
			case "loader":
				deps.legacy.loadConfig = nil
			case "load_failure":
				deps.legacy.loadConfig = func() (config.Config, error) { return conf, failure }
			case "runner":
				deps.runRuntime = nil
			case "notifier":
				deps.notifyContext = nil
			case "mq":
				conf.RABBITMQ = config.RabbitMQConfig{}
			}
			err := runProcess(deps)
			if err == nil || tc == "load_failure" && !errors.Is(err, failure) || tc == "mq" && !errors.Is(err, config.ErrRabbitMQNotConfigured) {
				t.Fatal("invalid process input error", err)
			}
			if tc == "context" && stops != 1 {
				t.Fatal("partial signal handle was not released")
			}
		})
	}
}

func TestRunProcessLegacyLifecycle(t *testing.T) {
	for _, mode := range []config.RuntimeMode{"", config.ModeLegacy} {
		t.Run("mode_"+string(mode), func(t *testing.T) {
			original := legacyState()
			conf := processTestConfig(mode)
			conf.LEGACY_WORKER.Enabled = true
			var events []string
			pool := sql.OpenDB(lifecycleConnector{close: func() error { events = append(events, "close_mysql"); return nil }})
			defer pool.Close()
			pool.SetMaxOpenConns(1)
			if err := pool.Ping(); err != nil {
				t.Fatal(err)
			}
			rds := redis.NewClient(&redis.Options{Dialer: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("test forbids Redis network")
			}, MaxRetries: -1})
			defer rds.Close()
			deps := processDependencies{factories: processTestFactories(), legacyOptions: defaultLegacyRuntimeOptions()}
			// The dispatcher still routes a single config snapshot; this test
			// uses the old compatibility bootstrap as its injected legacy runner.
			deps.runLegacy = func(_ context.Context, c config.Config, d startupDependencies, _ legacyRuntimeOptions) error {
				return bootstrapLegacy(c, d)
			}
			deps.notifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			}
			deps.legacy = startupDependencies{
				managed:    defaultLegacyRuntimeFactories(),
				loadConfig: func() (config.Config, error) { events = append(events, "config"); return conf, nil },
				openMySQL: func(got config.Config) (*gorm.DB, error) {
					events = append(events, "mysql")
					if got != conf {
						t.Fatal("legacy config snapshot changed")
					}
					return &gorm.DB{Config: &gorm.Config{ConnPool: pool}}, nil
				},
				openRedis: func(context.Context, config.Config) (*redis.Client, error) {
					events = append(events, "redis")
					return rds, nil
				},
				startWorker: func(scheduledtasks.MessageStore, *redis.Client) (func(), error) {
					events = append(events, "worker")
					return func() {
						events = append(events, "stop_worker")
						if pool.Ping() != nil || errors.Is(rds.Ping(context.Background()).Err(), redis.ErrClosed) {
							t.Error("storage closed before legacy worker joined")
						}
					}, nil
				},
				serve: func(got config.Config, _ *redis.Client, _ service.AccountUsers, _ service.ContactQueries, _ service.ContactWrites, _ service.MessageStore, _ service.MessageSubscriptions, storage router.StorageHealth) error {
					events = append(events, "http")
					if got != conf || storage == nil || reflect.DeepEqual(legacyState(), original) {
						t.Error("legacy dependencies not bound")
					}
					return nil
				},
			}
			if err := runProcess(deps); err != nil {
				t.Fatal(err)
			}
			want := []string{"config", "mysql", "redis", "worker", "http", "stop_worker", "close_mysql"}
			if !reflect.DeepEqual(events, want) || !reflect.DeepEqual(legacyState(), original) || !errors.Is(rds.Ping(context.Background()).Err(), redis.ErrClosed) {
				t.Fatal("legacy lifecycle changed", events)
			}
		})
	}
}
