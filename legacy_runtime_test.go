package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"iChat/application"
	"iChat/config"
	scheduledtasks "iChat/scheduledTasks"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

type legacyRuntimeTrace struct {
	mu     sync.Mutex
	events []string
}

func (t *legacyRuntimeTrace) add(s string) {
	t.mu.Lock()
	t.events = append(t.events, s)
	t.mu.Unlock()
}
func (t *legacyRuntimeTrace) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.events...)
}

type runtimeHTTPFixture struct {
	trace                   *legacyRuntimeTrace
	started, exit, requests chan struct{}
	exitOnce, requestsOnce  sync.Once
	serveErr                error
	drain, close            func(context.Context) error
}

func (h *runtimeHTTPFixture) Serve(l net.Listener) error {
	defer l.Close()
	h.trace.add("serve")
	close(h.started)
	<-h.exit
	return h.serveErr
}
func (h *runtimeHTTPFixture) StopAdmission() { h.trace.add("stop_http") }
func (h *runtimeHTTPFixture) Drain(ctx context.Context) error {
	h.trace.add("drain_http")
	if h.drain != nil {
		return h.drain(ctx)
	}
	return nil
}
func (h *runtimeHTTPFixture) Close(ctx context.Context) error {
	h.trace.add("close_http")
	if h.close != nil {
		return h.close(ctx)
	}
	h.finish()
	return nil
}
func (h *runtimeHTTPFixture) finish() {
	h.requestsOnce.Do(func() { close(h.requests) })
	h.exitOnce.Do(func() { close(h.exit) })
}
func (h *runtimeHTTPFixture) RequestsDone() <-chan struct{} { return h.requests }

type runtimeWorkerFixture struct {
	trace        *legacyRuntimeTrace
	done         chan struct{}
	once         sync.Once
	drain, close func(context.Context) error
	result       error
}

func (w *runtimeWorkerFixture) StopAdmission() { w.trace.add("stop_worker") }
func (w *runtimeWorkerFixture) Drain(ctx context.Context) error {
	w.trace.add("drain_worker")
	if w.drain != nil {
		return w.drain(ctx)
	}
	return nil
}
func (w *runtimeWorkerFixture) Close(ctx context.Context) error {
	w.trace.add("close_worker")
	if w.close != nil {
		return w.close(ctx)
	}
	w.finish()
	return w.result
}
func (w *runtimeWorkerFixture) finish()               { w.once.Do(func() { close(w.done) }) }
func (w *runtimeWorkerFixture) Done() <-chan struct{} { return w.done }
func (w *runtimeWorkerFixture) Wait(ctx context.Context) error {
	if err := waitLegacyWorker(ctx, w.done); err != nil {
		return err
	}
	return w.result
}

type runtimeHealthFixture struct {
	trace *legacyRuntimeTrace
	done  chan struct{}
	once  sync.Once
	stop  func(context.Context) error
}

func (*runtimeHealthFixture) StorageReady() bool { return true }
func (h *runtimeHealthFixture) StopContext(ctx context.Context) error {
	h.trace.add("stop_health")
	if h.stop != nil {
		return h.stop(ctx)
	}
	h.finish()
	return nil
}
func (h *runtimeHealthFixture) Done() <-chan struct{} { return h.done }
func (h *runtimeHealthFixture) finish()               { h.once.Do(func() { close(h.done) }) }

type legacyRuntimeFixture struct {
	d                startupDependencies
	c                config.Config
	o                legacyRuntimeOptions
	h                *runtimeHTTPFixture
	w                *runtimeWorkerFixture
	health           *runtimeHealthFixture
	trace            *legacyRuntimeTrace
	pool             *sql.DB
	rds              *redis.Client
	before           []any
	poolCloseErr     error
	redisTransferred bool
}

func newLegacyRuntimeFixture(t *testing.T) *legacyRuntimeFixture {
	t.Helper()
	f := &legacyRuntimeFixture{trace: &legacyRuntimeTrace{}, c: processTestConfig(config.ModeLegacy), o: defaultLegacyRuntimeOptions(), before: legacyState()}
	f.c.LEGACY_WORKER.Enabled = true
	f.o.Address = "127.0.0.1:0"
	f.h = &runtimeHTTPFixture{trace: f.trace, started: make(chan struct{}), exit: make(chan struct{}), requests: make(chan struct{}), serveErr: http.ErrServerClosed}
	f.w = &runtimeWorkerFixture{trace: f.trace, done: make(chan struct{})}
	f.health = &runtimeHealthFixture{trace: f.trace, done: make(chan struct{})}
	f.rds = redis.NewClient(&redis.Options{MaxRetries: -1, Dialer: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unit test forbids Redis network")
	}})
	f.pool = sql.OpenDB(lifecycleConnector{close: func() error {
		f.trace.add("close_mysql")
		if !reflect.DeepEqual(legacyState(), f.before) {
			t.Error("globals not restored before storage closed")
		}
		if f.redisTransferred && !errors.Is(f.rds.Ping(context.Background()).Err(), redis.ErrClosed) {
			t.Error("MySQL closed before Redis")
		}
		return f.poolCloseErr
	}})
	f.pool.SetMaxOpenConns(1)
	if err := f.pool.Ping(); err != nil {
		t.Fatal(err)
	}
	f.d = defaultStartupDependencies()
	f.d.loadConfig = func() (config.Config, error) { return f.c, nil }
	f.d.openMySQL = func(config.Config) (*gorm.DB, error) {
		f.trace.add("mysql")
		return &gorm.DB{Config: &gorm.Config{ConnPool: f.pool}}, nil
	}
	f.d.openRedis = func(context.Context, config.Config) (*redis.Client, error) {
		f.trace.add("redis")
		f.redisTransferred = true
		return f.rds, nil
	}
	f.d.startWorker = func(scheduledtasks.MessageStore, *redis.Client) (func(), error) {
		panic("managed runtime used legacy stop callback")
	}
	f.d.serve = nil // the managed entry must not require/use Gin.Run
	f.d.managed = legacyRuntimeFactories{
		HTTP: func(i legacyHTTPInputs) (legacyHTTPService, error) {
			f.trace.add("build_http")
			if i.Config != f.c || i.Redis != f.rds || i.Storage != f.health || i.Users == nil || i.Messages == nil || i.Subscriptions == nil {
				t.Error("HTTP did not receive initialized dependencies")
			}
			return f.h, nil
		},
		Worker: func(ctx context.Context, store scheduledtasks.MessageStore, rds *redis.Client, o legacyWorkerOptions) (legacyWorkerService, error) {
			f.trace.add("build_worker")
			if ctx.Err() != nil || store == nil || rds != f.rds || o != f.o.Worker {
				t.Error("worker dependency mismatch")
			}
			return f.w, nil
		},
		Health: func(*sql.DB) legacyHealthMonitor { f.trace.add("health"); return f.health },
		Listen: func(ctx context.Context, network, address string) (net.Listener, error) {
			f.trace.add("listen")
			return (&net.ListenConfig{}).Listen(ctx, network, address)
		},
	}
	t.Cleanup(func() { f.h.finish(); f.w.finish(); f.health.finish(); _ = f.rds.Close(); _ = f.pool.Close() })
	return f
}

func waitLegacyFixture(t *testing.T, l *legacyLifecycle) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := l.Wait(ctx)
	select {
	case <-l.Done():
	default:
		t.Fatal("lifecycle did not finish real cleanup", err)
	}
	return err
}

func TestLegacyRuntimeNormalAndDisabledWorker(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f := newLegacyRuntimeFixture(t)
			f.c.LEGACY_WORKER.Enabled = enabled
			if !enabled {
				f.d.managed.Worker = nil
				f.o.Worker = legacyWorkerOptions{}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			l, err := startLegacyLifecycle(ctx, f.c, f.d, f.o)
			if err != nil {
				t.Fatal(err)
			}
			legacyHTTPAwait(t, f.h.started)
			cancel()
			if err := waitLegacyFixture(t, l); err != nil {
				t.Fatal(err)
			}
			if err := l.Result(context.Background()); err != nil {
				t.Fatal(err)
			}
			events := f.trace.snapshot()
			position := func(s string) int {
				for i, e := range events {
					if e == s {
						return i
					}
				}
				return -1
			}
			for _, drain := range []string{"drain_http", "drain_worker"} {
				if !enabled && drain == "drain_worker" {
					continue
				}
				if position(drain) < position("stop_http") || enabled && position(drain) < position("stop_worker") {
					t.Fatal("drain began before all admission gates stopped", events)
				}
			}
			if position("close_http") > position("close_mysql") || position("stop_health") > position("close_mysql") || enabled && position("close_worker") > position("close_mysql") {
				t.Fatal("storage closed before components", events)
			}
			if !enabled && position("build_worker") >= 0 {
				t.Fatal("disabled worker was constructed")
			}
			if err := l.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLegacyRuntimeStartupRollback(t *testing.T) {
	for _, stage := range []string{"mysql_handle_error", "nil_mysql", "redis_handle_error", "nil_redis", "nil_health", "http_handle_error", "typed_nil_http", "listener_handle_error", "typed_nil_listener", "worker_handle_error", "nil_worker", "cancel_handoff"} {
		t.Run(stage, func(t *testing.T) {
			f := newLegacyRuntimeFixture(t)
			cause := errors.New("private startup detail")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch stage {
			case "mysql_handle_error":
				f.d.openMySQL = func(config.Config) (*gorm.DB, error) { return &gorm.DB{Config: &gorm.Config{ConnPool: f.pool}}, cause }
			case "nil_mysql":
				f.d.openMySQL = func(config.Config) (*gorm.DB, error) { return nil, nil }
			case "redis_handle_error":
				f.d.openRedis = func(context.Context, config.Config) (*redis.Client, error) {
					f.redisTransferred = true
					return f.rds, cause
				}
			case "nil_redis":
				f.d.openRedis = func(context.Context, config.Config) (*redis.Client, error) { return nil, nil }
			case "nil_health":
				f.d.managed.Health = func(*sql.DB) legacyHealthMonitor { return nil }
			case "http_handle_error":
				f.d.managed.HTTP = func(legacyHTTPInputs) (legacyHTTPService, error) { return f.h, cause }
			case "typed_nil_http":
				f.d.managed.HTTP = func(legacyHTTPInputs) (legacyHTTPService, error) { return (*runtimeHTTPFixture)(nil), nil }
			case "listener_handle_error":
				original := f.d.managed.Listen
				f.d.managed.Listen = func(ctx context.Context, n, a string) (net.Listener, error) {
					l, err := original(ctx, n, a)
					if err != nil {
						return l, err
					}
					return l, cause
				}
			case "typed_nil_listener":
				f.d.managed.Listen = func(context.Context, string, string) (net.Listener, error) { return (*net.TCPListener)(nil), nil }
			case "worker_handle_error":
				f.d.managed.Worker = func(context.Context, scheduledtasks.MessageStore, *redis.Client, legacyWorkerOptions) (legacyWorkerService, error) {
					return f.w, cause
				}
			case "nil_worker":
				f.d.managed.Worker = func(context.Context, scheduledtasks.MessageStore, *redis.Client, legacyWorkerOptions) (legacyWorkerService, error) {
					return nil, nil
				}
			case "cancel_handoff":
				original := f.d.managed.Worker
				f.d.managed.Worker = func(c context.Context, s scheduledtasks.MessageStore, r *redis.Client, o legacyWorkerOptions) (legacyWorkerService, error) {
					w, err := original(c, s, r, o)
					cancel()
					return w, err
				}
			}
			l, err := startLegacyLifecycle(ctx, f.c, f.d, f.o)
			if l == nil || !errors.Is(err, errLegacyRuntimeStart) {
				t.Fatal("startup failure lost owner", err)
			}
			final := waitLegacyFixture(t, l)
			if !errors.Is(final, errLegacyRuntimeStart) || strings.HasSuffix(stage, "handle_error") && !errors.Is(final, cause) {
				t.Fatal("rollback lost startup cause", final)
			}
			if !reflect.DeepEqual(legacyState(), f.before) {
				t.Fatal("failed startup left globals bound")
			}
			if l.listener != nil {
				_ = l.listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second))
				if conn, err := l.listener.Accept(); !errors.Is(err, net.ErrClosed) {
					if conn != nil {
						_ = conn.Close()
					}
					t.Fatal("idle listener leaked", err)
				}
			}
			select {
			case <-f.h.started:
				t.Fatal("startup failure activated HTTP")
			default:
			}
		})
	}
}

func TestLegacyRuntimePendingRetainsStorage(t *testing.T) {
	f := newLegacyRuntimeFixture(t)
	f.o.DrainTimeout, f.o.CloseTimeout = 25*time.Millisecond, 25*time.Millisecond
	drainContexts, closeContexts := make(chan context.Context, 2), make(chan context.Context, 3)
	f.h.drain = func(ctx context.Context) error { drainContexts <- ctx; <-ctx.Done(); return ctx.Err() }
	f.w.drain = f.h.drain
	f.h.close = func(ctx context.Context) error {
		closeContexts <- ctx
		f.h.exitOnce.Do(func() { close(f.h.exit) })
		<-ctx.Done()
		return ctx.Err()
	}
	f.w.close = func(ctx context.Context) error { closeContexts <- ctx; <-ctx.Done(); return ctx.Err() }
	f.health.stop = func(ctx context.Context) error { closeContexts <- ctx; f.health.finish(); return nil }
	f.w.result = errors.New("late release error SECRET_TOKEN")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, err := startLegacyLifecycle(ctx, f.c, f.d, f.o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { f.h.finish(); f.w.finish(); _ = waitLegacyFixture(t, l) }()
	legacyHTTPAwait(t, f.h.started)
	cancel()
	observer, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	first := l.Result(observer)
	if !errors.Is(first, errLegacyRuntimePending) || !errors.Is(first, context.DeadlineExceeded) {
		t.Fatal("pending work was reported as closed", first)
	}
	if a, b := <-drainContexts, <-drainContexts; a != b {
		t.Fatal("drain got separate per-component budgets")
	}
	a, b, c := <-closeContexts, <-closeContexts, <-closeContexts
	if a != b || b != c {
		t.Fatal("close got separate per-component budgets")
	}
	if reflect.DeepEqual(legacyState(), f.before) || f.pool.Ping() != nil || errors.Is(f.rds.Ping(context.Background()).Err(), redis.ErrClosed) {
		t.Fatal("pending tasks lost their globals/storage")
	}
	select {
	case <-l.Done():
		t.Fatal("pending owner reported cleanup complete")
	default:
	}
	expired, expire := context.WithCancel(context.Background())
	expire()
	if !errors.Is(l.Close(expired), context.Canceled) {
		t.Fatal("short observer did not return")
	}
	f.h.finish()
	f.w.finish()
	final := waitLegacyFixture(t, l)
	if !errors.Is(final, f.w.result) || !errors.Is(final, errLegacyRuntimePending) || l.Result(context.Background()) != first {
		t.Fatal("late result lost or bounded result mutated", final)
	}
	if strings.Contains(fmt.Sprintf("%#v", final), "SECRET_TOKEN") {
		t.Fatal("final error leaked dependency detail")
	}
}

func TestLegacyRuntimeUnexpectedServerExit(t *testing.T) {
	for _, serveErr := range []error{nil, http.ErrServerClosed, errors.New("accept failed")} {
		t.Run(fmt.Sprint(serveErr), func(t *testing.T) {
			f := newLegacyRuntimeFixture(t)
			f.h.serveErr = serveErr
			l, err := startLegacyLifecycle(context.Background(), f.c, f.d, f.o)
			if err != nil {
				t.Fatal(err)
			}
			legacyHTTPAwait(t, f.h.started)
			f.h.exitOnce.Do(func() { close(f.h.exit) })
			final := waitLegacyFixture(t, l)
			if !errors.Is(final, errLegacyRuntimeStopped) || serveErr != nil && !errors.Is(final, serveErr) {
				t.Fatal("unexpected server exit accepted", final)
			}
		})
	}
}

func TestLegacyRuntimePreflightAndSetupBudget(t *testing.T) {
	for _, invalid := range []string{"context", "cancelled", "modern", "options", "mysql", "redis", "http", "health", "listen", "worker", "worker_options"} {
		t.Run(invalid, func(t *testing.T) {
			f := newLegacyRuntimeFixture(t)
			var ctx context.Context = context.Background()
			switch invalid {
			case "context":
				ctx = nil
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "modern":
				f.c.RUNTIME.Mode = config.ModeAll
				f.c.LEGACY_WORKER.Enabled = false
			case "options":
				f.o.CloseTimeout = 0
			case "mysql":
				f.d.openMySQL = nil
			case "redis":
				f.d.openRedis = nil
			case "http":
				f.d.managed.HTTP = nil
			case "health":
				f.d.managed.Health = nil
			case "listen":
				f.d.managed.Listen = nil
			case "worker":
				f.d.managed.Worker = nil
			case "worker_options":
				f.o.Worker.Schedule = "SECRET_TOKEN"
			}
			l, err := startLegacyLifecycle(ctx, f.c, f.d, f.o)
			if l != nil || err == nil || len(f.trace.snapshot()) != 0 || strings.Contains(fmt.Sprintf("%#v", err), "SECRET_TOKEN") {
				t.Fatal("preflight performed I/O", err)
			}
			if invalid == "modern" && !errors.Is(err, application.ErrRuntimeNotAssembled) {
				t.Fatal("modern mode reached old runtime", err)
			}
		})
	}
	f := newLegacyRuntimeFixture(t)
	f.o.StartupTimeout = 20 * time.Millisecond
	original := f.d.managed.Listen
	f.d.managed.Listen = func(ctx context.Context, n, a string) (net.Listener, error) {
		listener, err := original(ctx, n, a)
		if err != nil {
			return listener, err
		}
		<-ctx.Done()
		return listener, ctx.Err()
	}
	l, err := startLegacyLifecycle(context.Background(), f.c, f.d, f.o)
	if l == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("setup budget not enforced", err)
	}
	if err := waitLegacyFixture(t, l); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("setup cause lost", err)
	}
}

func TestLegacyRuntimeIndependentErrors(t *testing.T) {
	f := newLegacyRuntimeFixture(t)
	serveErr, healthErr, workerErr, poolErr := errors.New("SECRET_TOKEN serve"), errors.New("SECRET_TOKEN health"), errors.New("SECRET_TOKEN worker"), errors.New("SECRET_TOKEN pool")
	f.h.serveErr = serveErr
	f.w.result = workerErr
	f.poolCloseErr = poolErr
	f.health.stop = func(context.Context) error { f.health.finish(); return healthErr }
	l, err := startLegacyLifecycle(context.Background(), f.c, f.d, f.o)
	if err != nil {
		t.Fatal(err)
	}
	legacyHTTPAwait(t, f.h.started)
	f.h.exitOnce.Do(func() { close(f.h.exit) })
	final := waitLegacyFixture(t, l)
	for _, cause := range []error{serveErr, healthErr, workerErr, poolErr} {
		if !errors.Is(final, cause) {
			t.Fatal("independent shutdown cause lost")
		}
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, final), "SECRET_TOKEN") {
			t.Fatal("formatted lifecycle error leaked cause")
		}
	}
}

func TestRunProcessLegacySignalDispatch(t *testing.T) {
	f := newLegacyRuntimeFixture(t)
	d := defaultProcessDependencies()
	d.legacy = f.d
	d.legacyOptions = f.o
	var processCtx context.Context
	var cancel context.CancelFunc
	registered := make(chan struct{})
	var mu sync.Mutex
	var observed bool
	d.notifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
		processCtx, cancel = context.WithCancel(context.Background())
		close(registered)
		return processCtx, func() { mu.Lock(); observed = true; mu.Unlock(); cancel() }
	}
	result := make(chan error, 1)
	go func() { result <- runProcess(d) }()
	legacyHTTPAwait(t, registered)
	legacyHTTPAwait(t, f.h.started)
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("entry did not join legacy runtime")
	}
	mu.Lock()
	defer mu.Unlock()
	if !observed || !reflect.DeepEqual(legacyState(), f.before) {
		t.Fatal("entry did not unregister after cleanup")
	}
}

func TestRunProcessLegacyPreflightBeforeSignals(t *testing.T) {
	for _, missing := range []string{"runner", "notifier", "factory", "options"} {
		t.Run(missing, func(t *testing.T) {
			f := newLegacyRuntimeFixture(t)
			d := defaultProcessDependencies()
			d.legacy = f.d
			d.legacyOptions = f.o
			d.notifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
				t.Fatal("invalid legacy dependencies registered signals")
				return nil, nil
			}
			switch missing {
			case "runner":
				d.runLegacy = nil
			case "notifier":
				d.notifyContext = nil
			case "factory":
				d.legacy.managed.HTTP = nil
			case "options":
				d.legacyOptions.CloseTimeout = 0
			}
			if err := runProcess(d); err == nil || len(f.trace.snapshot()) != 0 {
				t.Fatal("invalid legacy preflight started I/O", err)
			}
		})
	}
}
