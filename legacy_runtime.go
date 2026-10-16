package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"iChat/application"
	"iChat/config"
	"iChat/database"
	"iChat/health"
	"iChat/router"
	scheduledtasks "iChat/scheduledTasks"
	"iChat/service"
	"iChat/utils"

	"github.com/go-redis/redis/v8"
)

var (
	errLegacyRuntimeInput    = errors.New("invalid legacy runtime input")
	errLegacyRuntimeStart    = errors.New("legacy runtime startup failed")
	errLegacyRuntimeStopped  = errors.New("legacy HTTP server stopped unexpectedly")
	errLegacyRuntimeShutdown = errors.New("legacy runtime shutdown failed")
	errLegacyRuntimePending  = errors.New("legacy tasks still own storage after shutdown deadline")
)

type legacyRuntimeOptions struct {
	Address                                    string
	StartupTimeout, DrainTimeout, CloseTimeout time.Duration
	Worker                                     legacyWorkerOptions
}

func defaultLegacyRuntimeOptions() legacyRuntimeOptions {
	address := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		address = ":" + port // preserve Gin.Run's existing PORT behavior
	}
	return legacyRuntimeOptions{Address: address, StartupTimeout: 30 * time.Second, DrainTimeout: 10 * time.Second, CloseTimeout: 10 * time.Second, Worker: defaultLegacyWorkerOptions()}
}

type legacyHTTPService interface {
	Serve(net.Listener) error
	StopAdmission()
	Drain(context.Context) error
	Close(context.Context) error
	RequestsDone() <-chan struct{}
}
type legacyWorkerService interface {
	StopAdmission()
	Drain(context.Context) error
	Close(context.Context) error
	Done() <-chan struct{}
	Wait(context.Context) error
}
type legacyHealthMonitor interface {
	router.StorageHealth
	StopContext(context.Context) error
	Done() <-chan struct{}
}
type legacyHTTPInputs struct {
	Config        config.Config
	Redis         *redis.Client
	Users         service.AccountUsers
	Contacts      service.ContactQueries
	Writes        service.ContactWrites
	Messages      service.MessageStore
	Subscriptions service.MessageSubscriptions
	Storage       router.StorageHealth
}
type legacyRuntimeFactories struct {
	HTTP   func(legacyHTTPInputs) (legacyHTTPService, error)
	Worker func(context.Context, scheduledtasks.MessageStore, *redis.Client, legacyWorkerOptions) (legacyWorkerService, error)
	Health func(*sql.DB) legacyHealthMonitor
	Listen func(context.Context, string, string) (net.Listener, error)
}

func defaultLegacyRuntimeFactories() legacyRuntimeFactories {
	return legacyRuntimeFactories{
		HTTP: func(i legacyHTTPInputs) (legacyHTTPService, error) {
			tokens, err := utils.NewTokenService(i.Config, utils.NewRedisTokenBlacklist(i.Redis))
			if err != nil {
				return nil, err
			}
			return newLegacyHTTPServer(router.Router(i.Config, tokens, i.Users, i.Contacts, i.Writes, i.Messages, i.Subscriptions, i.Storage))
		},
		Worker: func(ctx context.Context, store scheduledtasks.MessageStore, rds *redis.Client, options legacyWorkerOptions) (legacyWorkerService, error) {
			return startLegacyWorker(ctx, store, rds, options)
		},
		Health: func(pool *sql.DB) legacyHealthMonitor {
			return health.StartStorage(pool.PingContext, 5*time.Second, 2*time.Second)
		},
		Listen: (&net.ListenConfig{}).Listen,
	}
}

func checkLegacyRuntime(c config.Config, d startupDependencies, o legacyRuntimeOptions) error {
	a, err := (application.RuntimeFactories{}).Check(c)
	if err != nil {
		return err
	}
	valid := func(t time.Duration) bool { return t > 0 && t <= time.Minute }
	if a.Mode() != config.ModeLegacy || o.Address == "" || !valid(o.StartupTimeout) || !valid(o.DrainTimeout) || !valid(o.CloseTimeout) ||
		d.openMySQL == nil || d.openRedis == nil || d.managed.HTTP == nil || d.managed.Listen == nil || d.managed.Health == nil {
		return errLegacyRuntimeInput
	}
	if c.LEGACY_WORKER.Enabled {
		if d.managed.Worker == nil {
			return errLegacyRuntimeInput
		}
		if _, err := parseLegacyWorkerOptions(o.Worker); err != nil {
			return err
		}
	}
	return nil
}

// legacyLifecycle must be the only owner of legacy global bindings. It keeps
// them and the storage pools alive if a bounded shutdown cannot join a task.
// Result reports the bounded attempt; Wait/Done observe eventual real cleanup.
type legacyLifecycle struct {
	options           legacyRuntimeOptions
	pool              *sql.DB
	redis             *redis.Client
	monitor           legacyHealthMonitor
	http              legacyHTTPService
	worker            legacyWorkerService
	listener          net.Listener
	restore           func()
	serveDone         chan struct{}
	serveErr          error // published by serveDone
	stop              chan struct{}
	stopOnce          sync.Once
	resultReady, done chan struct{}
	result, final     error // separately immutable, published by their channels
}

func startLegacyLifecycle(ctx context.Context, c config.Config, d startupDependencies, o legacyRuntimeOptions) (*legacyLifecycle, error) {
	if ctx == nil {
		return nil, errLegacyRuntimeInput
	}
	if err := checkLegacyRuntime(c, d, o); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, legacyRuntimeError(errLegacyRuntimeStart, "cancelled before setup", err)
	}
	l := &legacyLifecycle{options: o, stop: make(chan struct{}), resultReady: make(chan struct{}), done: make(chan struct{})}
	setup, cancel := context.WithTimeout(ctx, o.StartupTimeout)
	err := l.prepare(setup, c, d)
	cancel()
	if err != nil {
		go l.shutdown(err)
		return l, err
	}
	go func() {
		var cause error
		select {
		case <-ctx.Done():
		case <-l.stop:
		case <-l.serveDone:
			if ctx.Err() == nil {
				select {
				case <-l.stop:
				default:
					cause = legacyRuntimeError(errLegacyRuntimeStopped, "accept loop", l.serveErr)
				}
			}
		}
		l.shutdown(cause)
	}()
	return l, nil
}

func runLegacyRuntime(ctx context.Context, c config.Config, d startupDependencies, o legacyRuntimeOptions) error {
	l, err := startLegacyLifecycle(ctx, c, d, o)
	if l == nil {
		return err
	}
	return l.Result(context.Background())
}

func (l *legacyLifecycle) prepare(ctx context.Context, c config.Config, d startupDependencies) error {
	stage := "mysql"
	fail := func(err error) error { return legacyRuntimeError(errLegacyRuntimeStart, stage, err) }
	db, err := d.openMySQL(c)
	var poolErr error
	if db != nil && db.Config != nil {
		l.pool, poolErr = db.DB() // own a valid returned pool even on handle+error
	}
	if err != nil || poolErr != nil {
		return fail(errors.Join(err, poolErr))
	}
	if l.pool == nil {
		return fail(errLegacyRuntimeInput)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	stage = "redis"
	l.redis, err = d.openRedis(ctx, c)
	if err != nil {
		return fail(err)
	}
	if l.redis == nil {
		return fail(errLegacyRuntimeInput)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	l.restore = bindLegacyDependencies(c, db, l.redis)
	stage = "storage health"
	l.monitor = d.managed.Health(l.pool)
	if nilLegacyWorkerDependency(l.monitor) {
		l.monitor = nil
		return fail(errLegacyRuntimeInput)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	stage = "HTTP construction"
	l.http, err = d.managed.HTTP(legacyHTTPInputs{Config: c, Redis: l.redis, Users: database.Umanager,
		Contacts: service.ContactQueries{Friends: database.Rmanager, Groups: database.Gmanager}, Writes: service.ContactWrites{Friends: database.Rmanager, Groups: database.Gmanager},
		Messages: database.Mmanager, Subscriptions: database.Mmanager, Storage: l.monitor})
	if nilLegacyWorkerDependency(l.http) {
		l.http = nil
	}
	if err != nil {
		return fail(err)
	}
	if l.http == nil {
		return fail(errLegacyRuntimeInput)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	stage = "listener"
	l.listener, err = d.managed.Listen(ctx, "tcp", l.options.Address)
	if nilLegacyWorkerDependency(l.listener) {
		l.listener = nil
	}
	if err != nil {
		return fail(err)
	}
	if l.listener == nil {
		return fail(errLegacyRuntimeInput)
	}
	if c.LEGACY_WORKER.Enabled {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		stage = "worker registration"
		l.worker, err = d.managed.Worker(ctx, database.Mmanager, l.redis, l.options.Worker)
		if nilLegacyWorkerDependency(l.worker) {
			l.worker = nil
		}
		if err != nil {
			return fail(err)
		}
		if l.worker == nil {
			return fail(errLegacyRuntimeInput)
		}
	}
	stage = "activation handoff"
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	l.serveDone = make(chan struct{})
	go func() { l.serveErr = l.http.Serve(l.listener); close(l.serveDone) }()
	return nil
}

func (l *legacyLifecycle) Close(ctx context.Context) error {
	if ctx == nil {
		return errLegacyRuntimeInput
	}
	l.stopOnce.Do(func() { close(l.stop) })
	return l.Wait(ctx)
}
func (l *legacyLifecycle) Done() <-chan struct{} { return l.done }
func (l *legacyLifecycle) Result(ctx context.Context) error {
	if ctx == nil {
		return errLegacyRuntimeInput
	}
	if err := waitLegacyWorker(ctx, l.resultReady); err != nil {
		return err
	}
	return l.result
}
func (l *legacyLifecycle) Wait(ctx context.Context) error {
	if ctx == nil {
		return errLegacyRuntimeInput
	}
	if err := waitLegacyWorker(ctx, l.done); err != nil {
		return err
	}
	return l.final
}

func (l *legacyLifecycle) shutdown(cause error) {
	if l.http != nil {
		l.http.StopAdmission()
	}
	if l.worker != nil {
		l.worker.StopAdmission()
	}
	phase := func(timeout time.Duration, calls ...func(context.Context) error) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		errs := make([]error, len(calls))
		var group sync.WaitGroup
		for index, call := range calls {
			if call == nil {
				continue
			}
			group.Add(1)
			go func() { defer group.Done(); errs[index] = call(ctx) }()
		}
		group.Wait() // trusted callbacks must honor the common phase context
		return errors.Join(errs...)
	}
	var httpDrain, workerDrain, httpClose, workerClose, healthStop func(context.Context) error
	wrap := func(stage string, call func(context.Context) error) func(context.Context) error {
		return func(ctx context.Context) error { return legacyRuntimeError(errLegacyRuntimeShutdown, stage, call(ctx)) }
	}
	if l.http != nil {
		httpDrain = wrap("HTTP drain", l.http.Drain)
		httpClose = wrap("HTTP close", l.http.Close)
	}
	if l.worker != nil {
		workerDrain = wrap("worker drain", l.worker.Drain)
		workerClose = wrap("worker close", l.worker.Close)
	}
	if l.monitor != nil {
		healthStop = wrap("health stop", l.monitor.StopContext)
	}
	cause = errors.Join(cause, phase(l.options.DrainTimeout, httpDrain, workerDrain))
	if l.serveDone == nil && l.listener != nil {
		// Serve never received ownership on startup failure.
		cause = errors.Join(cause, legacyRuntimeError(errLegacyRuntimeShutdown, "idle listener close", l.listener.Close()))
	}
	var serveJoin func(context.Context) error
	if l.serveDone != nil {
		serveJoin = wrap("accept loop join", func(ctx context.Context) error { return waitLegacyWorker(ctx, l.serveDone) })
	}
	cause = errors.Join(cause, phase(l.options.CloseTimeout, httpClose, workerClose, healthStop, serveJoin))
	var pending []<-chan struct{}
	if l.http != nil {
		pending = append(pending, l.http.RequestsDone())
	}
	if l.worker != nil {
		pending = append(pending, l.worker.Done())
	}
	if l.monitor != nil {
		pending = append(pending, l.monitor.Done())
	}
	if l.serveDone != nil {
		pending = append(pending, l.serveDone)
	}
	joined := true
	for _, done := range pending {
		select {
		case <-done:
		default:
			joined = false
		}
	}
	if !joined {
		cause = errors.Join(cause, legacyRuntimeError(errLegacyRuntimePending, "join pending", context.DeadlineExceeded))
		l.result = cause
		close(l.resultReady)
		// This same owner retains globals and pools. It never lets a short
		// observer tear them down beneath still-running handlers/transactions.
		for _, done := range pending {
			<-done
		}
	}
	// Preserve a late release failure even if its first Close observer expired.
	if l.worker != nil {
		cause = errors.Join(cause, legacyRuntimeError(errLegacyRuntimeShutdown, "worker final result", l.worker.Wait(context.Background())))
	}
	if l.restore != nil {
		l.restore()
	}
	if l.redis != nil {
		cause = errors.Join(cause, legacyRuntimeError(errLegacyRuntimeShutdown, "redis close", l.redis.Close()))
	}
	if l.pool != nil {
		cause = errors.Join(cause, legacyRuntimeError(errLegacyRuntimeShutdown, "mysql close", l.pool.Close()))
	}
	l.final = cause
	close(l.done)
	if joined {
		l.result = cause
		close(l.resultReady)
	}
}

type legacyRuntimeFailure struct {
	kind  error
	stage string
	cause error
}

func (e *legacyRuntimeFailure) Error() string    { return e.kind.Error() + ": " + e.stage }
func (e *legacyRuntimeFailure) String() string   { return e.Error() }
func (e *legacyRuntimeFailure) GoString() string { return e.Error() }
func (e *legacyRuntimeFailure) Unwrap() []error  { return []error{e.kind, e.cause} }
func legacyRuntimeError(kind error, stage string, cause error) error {
	if cause == nil && kind != errLegacyRuntimeStart && kind != errLegacyRuntimeStopped {
		return nil
	}
	return &legacyRuntimeFailure{kind: kind, stage: stage, cause: cause}
}
