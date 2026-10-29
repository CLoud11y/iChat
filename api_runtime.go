package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"iChat/application"
	"iChat/config"
	"iChat/database"
	"iChat/health"
	"iChat/middlewares"
	"iChat/router"
	"iChat/service"
	"iChat/utils"
)

var (
	errAPIInput    = errors.New("invalid API configuration or dependencies")
	errAPIStart    = errors.New("API activation failed")
	errAPIStopped  = errors.New("API stopped unexpectedly")
	errAPIShutdown = errors.New("API cleanup failed")
	errAPIPending  = errors.New("API work still owns storage")
)

type apiOptions struct {
	Address                                                                    string
	ReadHeaderTimeout, ReadTimeout, WriteTimeout, IdleTimeout                  time.Duration
	SubmissionTimeout, ProbeInterval, ProbeTimeout, DrainTimeout, CloseTimeout time.Duration
	StatusTimeout, ConversationsTimeout                                        time.Duration
	MaxHeaderBytes                                                             int
}

func defaultAPIOptions(c config.Config) apiOptions {
	address := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		address = ":" + port
	}
	return apiOptions{Address: address, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: max(30*time.Second, 11*time.Second+c.RABBITMQ.PublishTimeout), IdleTimeout: 30 * time.Second,
		SubmissionTimeout: c.RABBITMQ.PublishTimeout, StatusTimeout: service.DefaultCommandStatusTimeout, ConversationsTimeout: service.DefaultConversationsTimeout, ProbeInterval: 2 * time.Second, ProbeTimeout: time.Second, DrainTimeout: 10 * time.Second, CloseTimeout: 5 * time.Second, MaxHeaderBytes: 32 * 1024}
}

type apiSQL interface {
	PingContext(context.Context) error
	Close() error
}
type apiRedis interface {
	Get(context.Context, string) *redis.StringCmd
	Set(context.Context, string, interface{}, time.Duration) *redis.StatusCmd
	Close() error
}
type apiMonitor interface {
	router.StorageHealth
	StopContext(context.Context) error
	Done() <-chan struct{}
}
type apiDependencies struct {
	newSQL        func(config.Config) (apiSQL, error)
	newRedis      func(config.Config, apiOptions) (apiRedis, error)
	chat          func(apiSQL, apiRedis) (*service.ReliableChat, error)
	status        func(apiSQL) (service.CommandStatusReader, error)
	conversations func(apiSQL) (service.ConversationReader, error)
	tokens        func(config.Config, apiRedis) (*utils.TokenService, error)
	monitor       func(func(context.Context) error, time.Duration, time.Duration) apiMonitor
	http          func(http.Handler, apiOptions) (*legacyHTTPServer, error)
	listen        func(context.Context, string) (net.Listener, error)
}

func defaultAPIDependencies() apiDependencies {
	return apiDependencies{
		conversations: func(handle apiSQL) (service.ConversationReader, error) {
			pool, ok := handle.(*sql.DB)
			if !ok {
				return nil, errAPIInput
			}
			return database.NewMessageRepository(pool)
		},
		status: func(handle apiSQL) (service.CommandStatusReader, error) {
			pool, ok := handle.(*sql.DB)
			if !ok {
				return nil, errAPIInput
			}
			return database.NewMessageRepository(pool)
		},
		chat: func(sqlHandle apiSQL, redisHandle apiRedis) (*service.ReliableChat, error) {
			pool, ok := sqlHandle.(*sql.DB)
			if !ok {
				return nil, errAPIInput
			}
			client, ok := redisHandle.(*redis.Client)
			if !ok {
				return nil, errAPIInput
			}
			source, err := database.NewNotificationSource(pool, client)
			if err != nil {
				return nil, err
			}
			reader, err := database.NewMessageRepository(pool)
			if err != nil {
				return nil, err
			}
			return service.NewReliableChat(source, reader, reader, service.DefaultReliableChatOptions())
		},
		newSQL: func(c config.Config) (apiSQL, error) {
			db, err := utils.NewMySQL(c) // Lazy; no schema migration or global binding.
			if err != nil {
				return nil, err
			}
			pool, err := db.DB()
			if err == nil {
				pool.SetMaxOpenConns(4)
				pool.SetMaxIdleConns(2)
			}
			return pool, err
		},
		newRedis: func(c config.Config, o apiOptions) (apiRedis, error) {
			// Lazy, bounded reads; v8 honors the earlier context/socket deadline.
			// No idle background dialers and no implicit GET retry amplification.
			return redis.NewClient(&redis.Options{Addr: c.REDIS.Addr, Password: c.REDIS.Password, DB: c.REDIS.DB, PoolSize: c.REDIS.PoolSize, MinIdleConns: 0, MaxRetries: -1,
				DialTimeout: o.ProbeTimeout, ReadTimeout: o.ProbeTimeout, WriteTimeout: o.ProbeTimeout, PoolTimeout: o.ProbeTimeout}), nil
		},
		tokens: func(c config.Config, client apiRedis) (*utils.TokenService, error) {
			rds, ok := client.(*redis.Client)
			if !ok {
				return nil, errAPIInput
			}
			return utils.NewTokenService(c, utils.NewRedisTokenBlacklist(rds))
		},
		monitor: func(probe func(context.Context) error, interval, timeout time.Duration) apiMonitor {
			return health.StartStorage(probe, interval, timeout)
		},
		http: func(handler http.Handler, o apiOptions) (*legacyHTTPServer, error) {
			server, err := newLegacyHTTPServer(handler) // Reuse transport ownership only.
			if err != nil {
				return nil, err
			}
			server.server.ReadHeaderTimeout, server.server.ReadTimeout, server.server.WriteTimeout, server.server.IdleTimeout = o.ReadHeaderTimeout, o.ReadTimeout, o.WriteTimeout, o.IdleTimeout
			server.server.MaxHeaderBytes = o.MaxHeaderBytes
			server.requests.rejected = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(503)
				if req.Method == http.MethodPost && req.URL.Path == "/auth/sendMessage" {
					io.WriteString(w, `{"code":-1,"msg":"API admission stopped","data":{"state":"not_published","reasonCode":"DEPENDENCY_UNAVAILABLE"}}`)
				} else {
					io.WriteString(w, `{"code":-1,"msg":"API admission stopped","data":{"reasonCode":"DEPENDENCY_UNAVAILABLE"}}`)
				}
			})
			return server, nil
		},
		listen: func(ctx context.Context, address string) (net.Listener, error) {
			return (&net.ListenConfig{}).Listen(ctx, "tcp", address)
		},
	}
}

func checkAPI(c config.Config, o apiOptions, d apiDependencies) error {
	plan, planErr := c.RuntimePlan()
	if planErr != nil {
		return planErr
	}
	if !plan.API {
		return errAPIInput
	}
	valid := func(v time.Duration) bool { return v > 0 && v <= time.Minute }
	_, port, err := net.SplitHostPort(o.Address)
	p, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || p < 0 || p > 65535 || !valid(o.ReadHeaderTimeout) || !valid(o.ReadTimeout) || !valid(o.IdleTimeout) || !valid(o.SubmissionTimeout) || !valid(o.StatusTimeout) || !valid(o.ConversationsTimeout) || !valid(o.ProbeInterval) || !valid(o.ProbeTimeout) || !valid(o.DrainTimeout) || !valid(o.CloseTimeout) || o.WriteTimeout < o.ReadTimeout+max(o.SubmissionTimeout, o.StatusTimeout, o.ConversationsTimeout) || o.WriteTimeout > 2*time.Minute || o.ReadHeaderTimeout > o.ReadTimeout || o.MaxHeaderBytes < 4096 || o.MaxHeaderBytes > 64*1024 {
		return errAPIInput
	}
	if d.newSQL == nil || d.newRedis == nil || d.chat == nil || d.status == nil || d.conversations == nil || d.tokens == nil || d.monitor == nil || d.http == nil || d.listen == nil {
		return errAPIInput
	}
	if _, err := utils.NewTokenService(c, utils.NewRedisTokenBlacklist(nil)); err != nil {
		return apiError(errAPIInput, "authentication configuration", err)
	}
	_, redisPort, redisAddressErr := net.SplitHostPort(c.REDIS.Addr)
	redisPortNumber, redisPortErr := strconv.Atoi(redisPort)
	if redisAddressErr != nil || redisPortErr != nil || redisPortNumber < 1 || redisPortNumber > 65535 || c.REDIS.DB < 0 || c.REDIS.PoolSize < 1 || c.REDIS.PoolSize > 1024 || c.REDIS.MinIdleConn < 0 || c.REDIS.MinIdleConn > c.REDIS.PoolSize {
		return errAPIInput
	}
	mysqlPort, mysqlPortErr := strconv.Atoi(c.MYSQL.Port)
	if c.MYSQL.Host == "" || mysqlPortErr != nil || mysqlPort < 1 || mysqlPort > 65535 || c.MYSQL.User == "" || c.MYSQL.DbName == "" {
		return errAPIInput
	}
	return nil
}

type apiAssembly struct {
	options      func(config.Config) apiOptions
	dependencies apiDependencies
}

type apiResources struct {
	sql           apiSQL
	redis         apiRedis
	auth, storage apiMonitor
	http          *legacyHTTPServer
	listener      net.Listener
}

// apiService owns its requests, probes and clients; shared MQ remains owned by
// RunRuntime. Construction is idle. A single cleanup owner retains clients even
// if Close's observer expires while a request/probe still uses them.
type apiService struct {
	mu                               sync.Mutex
	conf                             config.Config
	inputs                           application.RoleDependencies
	options                          apiOptions
	deps                             apiDependencies
	started, stopped, draining       bool
	setupCancel                      context.CancelFunc
	workCancel                       context.CancelFunc
	resources                        *apiResources
	prepared, serveDone, force, done chan struct{}
	forceOnce                        sync.Once
	serveErr, startErr, finalErr     error
	admitting                        atomic.Bool
}

func newAPIService(c config.Config, inputs application.RoleDependencies, o apiOptions, d apiDependencies) (*apiService, error) {
	if err := checkAPI(c, o, d); err != nil {
		return nil, err
	}
	if apiNil(inputs.Publisher) || apiNil(inputs.Health) || len(inputs.Consumers) != 0 {
		return nil, errAPIInput
	}
	return &apiService{conf: c, inputs: inputs, options: o, deps: d, prepared: make(chan struct{}), serveDone: make(chan struct{}), force: make(chan struct{}), done: make(chan struct{})}, nil
}

func (*apiService) String() string     { return "owned command API service" }
func (s *apiService) GoString() string { return s.String() }

func (s *apiService) Start(setup, lifetime context.Context) (err error) {
	if setup == nil || lifetime == nil {
		return errAPIInput
	}
	if err := errors.Join(setup.Err(), lifetime.Err()); err != nil {
		return apiError(errAPIStart, "startup context", err)
	}
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return errAPIInput
	}
	s.started = true
	ctx, cancel := context.WithCancel(setup)
	s.setupCancel = cancel
	s.mu.Unlock()
	resources := &apiResources{}
	go s.manage(lifetime)
	defer func() {
		s.mu.Lock()
		if err == nil && ctx.Err() != nil {
			err = apiError(errAPIStart, "setup handoff", ctx.Err())
		}
		if err == nil && lifetime.Err() != nil {
			err = apiError(errAPIStart, "lifetime handoff", lifetime.Err())
		}
		if err == nil && s.stopped {
			err = apiError(errAPIStart, "stopped handoff", nil)
		}
		s.resources, s.startErr, s.setupCancel = resources, err, nil
		if err == nil {
			s.admitting.Store(true)
			go func() {
				serveErr := resources.http.Serve(resources.listener)
				s.mu.Lock()
				s.serveErr = serveErr
				s.mu.Unlock()
				close(s.serveDone)
			}()
		} else {
			close(s.serveDone)
		}
		close(s.prepared)
		s.mu.Unlock()
		cancel()
		if err != nil {
			s.forceOnce.Do(func() { close(s.force) })
		}
	}()
	if err := ctx.Err(); err != nil {
		return apiError(errAPIStart, "startup context", err)
	}
	resources.sql, err = s.deps.newSQL(s.conf)
	if err != nil || apiNil(resources.sql) {
		return apiError(errAPIStart, "storage pool", err)
	}
	if err := ctx.Err(); err != nil {
		return apiError(errAPIStart, "startup context", err)
	}
	resources.redis, err = s.deps.newRedis(s.conf, s.options)
	if err != nil || apiNil(resources.redis) {
		return apiError(errAPIStart, "authentication client", err)
	}
	tokens, err := s.deps.tokens(s.conf, resources.redis)
	if err != nil || tokens == nil {
		return apiError(errAPIStart, "token service", err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return apiError(errAPIStart, "probe identity", err)
	}
	probeKey := "ichat:auth:probe:" + hex.EncodeToString(nonce[:])
	resources.auth = s.deps.monitor(func(ctx context.Context) error {
		err := resources.redis.Get(ctx, probeKey).Err()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		return err
	}, s.options.ProbeInterval, s.options.ProbeTimeout)
	if apiNil(resources.auth) {
		return apiError(errAPIStart, "authentication probe", nil)
	}
	resources.storage = s.deps.monitor(resources.sql.PingContext, s.options.ProbeInterval, s.options.ProbeTimeout)
	if apiNil(resources.storage) {
		return apiError(errAPIStart, "storage probe", nil)
	}
	r := gin.New()
	r.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ interface{}) {
		data := gin.H{"reasonCode": "DEPENDENCY_UNAVAILABLE"}
		if c.Request.Method == http.MethodPost && c.Request.URL.Path == "/auth/sendMessage" {
			data = gin.H{"state": "publish_unknown", "reasonCode": "PUBLISH_UNKNOWN"}
		}
		c.Header("Cache-Control", "no-store")
		c.AbortWithStatusJSON(503, gin.H{"code": -1, "msg": "API request failed", "data": data})
	}), middlewares.Cors())
	router.RegisterHealth(r, resources.storage, s.inputs.Health)
	if err := router.RegisterSubmission(r, tokens, s.inputs.Publisher, s.options.SubmissionTimeout); err != nil {
		return apiError(errAPIStart, "submission routes", err)
	}
	statusReader, err := s.deps.status(resources.sql)
	if err != nil || apiNil(statusReader) {
		return apiError(errAPIStart, "status assembly", err)
	}
	if err := router.RegisterCommandStatus(r, tokens, statusReader, s.options.StatusTimeout); err != nil {
		return apiError(errAPIStart, "status routes", err)
	}
	conversationReader, err := s.deps.conversations(resources.sql)
	if err != nil || apiNil(conversationReader) {
		return apiError(errAPIStart, "conversations assembly", err)
	}
	if err := router.RegisterConversations(r, tokens, conversationReader, s.options.ConversationsTimeout); err != nil {
		return apiError(errAPIStart, "conversations routes", err)
	}
	chat, err := s.deps.chat(resources.sql, resources.redis)
	if err != nil || chat == nil {
		return apiError(errAPIStart, "chat assembly", err)
	}
	if err := router.RegisterReliableChat(r, tokens, chat); err != nil {
		return apiError(errAPIStart, "chat routes", err)
	}
	resources.http, err = s.deps.http(r, s.options)
	if err != nil || resources.http == nil {
		return apiError(errAPIStart, "HTTP owner", err)
	}
	work, cancelWork := context.WithCancel(context.Background())
	s.mu.Lock()
	s.workCancel = cancelWork
	s.mu.Unlock()
	resources.http.server.BaseContext = func(net.Listener) context.Context { return work }
	if err := ctx.Err(); err != nil {
		return apiError(errAPIStart, "startup context", err)
	}
	resources.listener, err = s.deps.listen(ctx, s.options.Address)
	if err != nil || apiNil(resources.listener) {
		return apiError(errAPIStart, "listener", err)
	}
	return nil
}

func (s *apiService) AcceptanceReady() bool {
	if !s.admitting.Load() {
		return false
	}
	select {
	case <-s.serveDone:
		return false
	default:
		return true
	}
}
func (s *apiService) AuthenticationReady() bool {
	s.mu.Lock()
	resources, stopped := s.resources, s.stopped
	s.mu.Unlock()
	return !stopped && resources != nil && !apiNil(resources.auth) && resources.auth.StorageReady()
}
func (s *apiService) StopAdmission() {
	s.admitting.Store(false)
	s.mu.Lock()
	s.stopped = true
	resources, cancel := s.resources, s.setupCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if resources != nil && resources.http != nil {
		resources.http.StopAdmission()
	}
}
func (s *apiService) Drain(ctx context.Context) error {
	if ctx == nil {
		return errAPIInput
	}
	s.StopAdmission()
	s.mu.Lock()
	s.draining = true
	started := s.started
	s.mu.Unlock()
	if !started {
		return nil
	}
	select {
	case <-s.prepared:
	case <-ctx.Done():
		return apiError(errAPIShutdown, "drain setup", ctx.Err())
	}
	s.mu.Lock()
	resources := s.resources
	s.mu.Unlock()
	if resources != nil && resources.http != nil {
		return resources.http.Drain(ctx)
	}
	return nil
}
func (s *apiService) Close(ctx context.Context) error {
	if ctx == nil {
		return errAPIInput
	}
	s.StopAdmission()
	s.mu.Lock()
	if !s.started {
		s.started = true
		close(s.prepared)
		close(s.serveDone)
		go s.manage(context.Background())
	}
	s.mu.Unlock()
	s.forceOnce.Do(func() { close(s.force) })
	select {
	case <-s.done:
		return s.Err()
	default:
	}
	select {
	case <-s.done:
		return s.Err()
	case <-ctx.Done():
		return apiError(errAPIPending, "close observer", ctx.Err())
	}
}
func (s *apiService) Done() <-chan struct{} { return s.done }
func (s *apiService) Err() error            { s.mu.Lock(); defer s.mu.Unlock(); return s.finalErr }

func (s *apiService) manage(lifetime context.Context) {
	<-s.prepared
	s.mu.Lock()
	resources, startErr := s.resources, s.startErr
	s.mu.Unlock()
	var result error
	if startErr != nil {
		result = startErr
	} else {
		select {
		case <-s.force:
		case <-lifetime.Done():
		case <-s.serveDone:
			s.mu.Lock()
			expected, serveErr := s.draining || s.stopped, s.serveErr
			s.mu.Unlock()
			if expected && errors.Is(serveErr, http.ErrServerClosed) {
				select {
				case <-s.force:
				case <-lifetime.Done():
				}
			} else if resources != nil {
				result = apiError(errAPIStopped, "accept loop", serveErr)
				drain, cancel := context.WithTimeout(context.Background(), s.options.DrainTimeout)
				result = errors.Join(result, s.Drain(drain))
				cancel()
			}
		}
	}
	s.StopAdmission()
	s.mu.Lock()
	cancelWork := s.workCancel
	s.mu.Unlock()
	if cancelWork != nil {
		cancelWork()
	}
	cleanup, cancel := context.WithTimeout(context.Background(), s.options.CloseTimeout)
	var wg sync.WaitGroup
	var failures [3]error
	if resources != nil {
		if resources.http != nil {
			wg.Add(1)
			go func() { defer wg.Done(); failures[0] = resources.http.Close(cleanup) }()
		}
		for i, probe := range []apiMonitor{resources.auth, resources.storage} {
			if apiNil(probe) {
				continue
			}
			wg.Add(1)
			go func() { defer wg.Done(); failures[i+1] = probe.StopContext(cleanup) }()
		}
	}
	wg.Wait()
	cancel()
	result = errors.Join(result, apiError(errAPIShutdown, "components", errors.Join(failures[:]...)))
	if resources != nil {
		// Keep the same owner and storage alive until real completion, even
		// when an observer or cooperative close callback has timed out.
		if resources.http != nil {
			<-resources.http.RequestsDone()
		}
		for _, probe := range []apiMonitor{resources.auth, resources.storage} {
			if !apiNil(probe) {
				<-probe.Done()
			}
		}
		if !apiNil(resources.listener) {
			// Serve owns the listener only after successful Start handoff.
			if startErr != nil {
				result = errors.Join(result, apiError(errAPIShutdown, "idle listener", resources.listener.Close()))
			}
		}
		<-s.serveDone
		if !apiNil(resources.redis) {
			result = errors.Join(result, apiError(errAPIShutdown, "authentication client", resources.redis.Close()))
		}
		if !apiNil(resources.sql) {
			result = errors.Join(result, apiError(errAPIShutdown, "storage pool", resources.sql.Close()))
		}
	}
	s.mu.Lock()
	s.finalErr = result
	s.mu.Unlock()
	close(s.done)
}

type apiFailure struct {
	kind  error
	stage string
	cause error
}

func (e *apiFailure) Error() string    { return e.kind.Error() + ": " + e.stage }
func (e *apiFailure) String() string   { return e.Error() }
func (e *apiFailure) GoString() string { return e.Error() }
func (e *apiFailure) Unwrap() []error  { return []error{e.kind, e.cause} }
func apiError(kind error, stage string, cause error) error {
	if kind == errAPIShutdown && cause == nil {
		return nil
	}
	return &apiFailure{kind, stage, cause}
}
func apiNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
