package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"iChat/application"
	"iChat/config"
	"iChat/health"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/service"
	"iChat/utils"
)

func apiTestConfig() config.Config {
	c := processTestConfig(config.ModeAPI)
	c.JWT.Key, c.JWT.TokenHourLifeSpan = "api-lifecycle-fixture", 1
	c.REDIS.Addr, c.REDIS.PoolSize = "127.0.0.1:9", 4
	c.MYSQL.Host, c.MYSQL.Port, c.MYSQL.User, c.MYSQL.DbName = "127.0.0.1", "9", "api_fixture", "api_fixture"
	return c
}
func apiTestOptions() apiOptions {
	o := defaultAPIOptions(apiTestConfig())
	o.Address = "127.0.0.1:0"
	o.ReadHeaderTimeout, o.ReadTimeout, o.SubmissionTimeout, o.WriteTimeout, o.IdleTimeout = 60*time.Millisecond, 120*time.Millisecond, 200*time.Millisecond, time.Second, 120*time.Millisecond
	o.StatusTimeout = 200 * time.Millisecond
	o.ConversationsTimeout = 200 * time.Millisecond
	o.SyncTimeout = 200 * time.Millisecond
	o.ProbeInterval, o.ProbeTimeout, o.CloseTimeout, o.DrainTimeout = 10*time.Millisecond, 50*time.Millisecond, 200*time.Millisecond, 300*time.Millisecond
	return o
}

type apiFakeSQL struct {
	closed  atomic.Int32
	offline atomic.Bool
	probe   func(context.Context) error
	onClose func()
}

func (s *apiFakeSQL) PingContext(ctx context.Context) error {
	if s.probe != nil {
		return s.probe(ctx)
	}
	if s.closed.Load() != 0 || s.offline.Load() {
		return errors.New("PRIVATE_SQL_CAUSE")
	}
	return ctx.Err()
}
func (s *apiFakeSQL) Close() error {
	if s.onClose != nil {
		s.onClose()
	}
	s.closed.Add(1)
	return nil
}

type apiFakeRedis struct {
	closed  atomic.Int32
	offline atomic.Bool
	gets    atomic.Int32
	onClose func()
}

func (r *apiFakeRedis) Get(ctx context.Context, key string) *redis.StringCmd {
	r.gets.Add(1)
	cmd := redis.NewStringCmd(ctx, "get", key)
	if r.closed.Load() != 0 || r.offline.Load() {
		cmd.SetErr(errors.New("PRIVATE_REDIS_CAUSE"))
	} else {
		cmd.SetErr(redis.Nil)
	}
	return cmd
}
func (*apiFakeRedis) Set(ctx context.Context, key string, value interface{}, ttl time.Duration) *redis.StatusCmd {
	return redis.NewStatusCmd(ctx)
}
func (r *apiFakeRedis) Close() error {
	if r.onClose != nil {
		r.onClose()
	}
	r.closed.Add(1)
	return nil
}

type apiFakeBlacklist struct{ redis apiRedis }

func (b apiFakeBlacklist) Contains(ctx context.Context, key string) (bool, error) {
	err := b.redis.Get(ctx, key).Err()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	return err == nil, err
}
func (apiFakeBlacklist) Add(context.Context, string, time.Duration) error { return nil }

type apiPublisher struct {
	calls   atomic.Int32
	publish func(context.Context, messaging.Command, time.Time) (rabbitmq.PublishResult, error)
}

func (p *apiPublisher) Publish(ctx context.Context, c messaging.Command, at time.Time) (rabbitmq.PublishResult, error) {
	p.calls.Add(1)
	if p.publish != nil {
		return p.publish(ctx, c, at)
	}
	e, err := messaging.NewEnvelope(c, at)
	return rabbitmq.PublishResult{Outcome: rabbitmq.PublishAccepted, CommandID: c.CommandID, PublishAttemptID: e.PublishAttemptID, ConnectionEpoch: 1, ChannelEpoch: 1, DeliveryTag: 1}, err
}
func (*apiPublisher) Status() rabbitmq.SupervisorStatus {
	return rabbitmq.SupervisorStatus{State: rabbitmq.SupervisorReady, Ready: true, ConnectionEpoch: 1}
}
func (*apiPublisher) WaitReady(context.Context) (uint64, error) {
	panic("API activation/health must not wait for publisher")
}

type apiTestHealth struct{}

func (apiTestHealth) RuntimeReadiness() health.RuntimeStatus {
	return health.RuntimeStatus{Acceptance: health.Ready}
}

type apiHarness struct {
	c         config.Config
	o         apiOptions
	d         apiDependencies
	sql       *apiFakeSQL
	redis     *apiFakeRedis
	publisher *apiPublisher
}

func newAPIHarness() *apiHarness {
	h := &apiHarness{c: apiTestConfig(), o: apiTestOptions(), sql: &apiFakeSQL{}, redis: &apiFakeRedis{}, publisher: &apiPublisher{}}
	h.d = defaultAPIDependencies()
	h.d.newSQL = func(config.Config) (apiSQL, error) { return h.sql, nil }
	h.d.newRedis = func(config.Config, apiOptions) (apiRedis, error) { return h.redis, nil }
	h.d.chat = apiFixtureChat
	h.d.status = apiFixtureStatus
	h.d.conversations = apiFixtureConversations
	h.d.syncMessages = apiFixtureSync
	h.d.tokens = func(c config.Config, r apiRedis) (*utils.TokenService, error) {
		return utils.NewTokenService(c, apiFakeBlacklist{r})
	}
	return h
}
func (h *apiHarness) service(t *testing.T) *apiService {
	t.Helper()
	s, err := newAPIService(h.c, application.RoleDependencies{Publisher: h.publisher, Health: apiTestHealth{}}, h.o, h.d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Close(ctx)
	})
	return s
}
func apiWait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("API lifecycle event did not complete")
	}
}
func apiEventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !f() {
		select {
		case <-deadline.C:
			t.Fatal("API state did not converge")
		case <-ticker.C:
		}
	}
}
func apiAddress(s *apiService) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resources.listener.Addr().String()
}

const apiTestBody = `{"clientMessageId":"8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1","targetType":"private","targetId":"34","media":"text","content":"api lifecycle"}`

func apiSend(t *testing.T, address, token string) (int, string) {
	t.Helper()
	request, _ := http.NewRequest("POST", "http://"+address+"/auth/sendMessage", strings.NewReader(apiTestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}
func apiToken(t *testing.T, h *apiHarness) string {
	t.Helper()
	tokens, err := h.d.tokens(h.c, h.redis)
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokens.GenerateToken(42)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAPIActivationIsIdleAndSetupCancellationDoesNotOwnLifetime(t *testing.T) {
	before := legacyState()
	h := newAPIHarness()
	s := h.service(t)
	if h.redis.gets.Load() != 0 || s.AcceptanceReady() || s.AuthenticationReady() {
		t.Fatal("constructor activated API/probe")
	}
	setup, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(setup, context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	apiEventually(t, s.AuthenticationReady)
	if code, body := apiSend(t, apiAddress(s), apiToken(t, h)); code != 202 || !strings.Contains(body, `"state":"accepted"`) {
		t.Fatal(code, body)
	}
	h.redis.offline.Store(true)
	apiEventually(t, func() bool { return !s.AuthenticationReady() })
	if code, body := apiSend(t, apiAddress(s), apiToken(t, h)); code != 503 || strings.Contains(body, "PRIVATE") {
		t.Fatal(code, body)
	}
	h.redis.offline.Store(false)
	apiEventually(t, s.AuthenticationReady)
	if h.publisher.calls.Load() != 1 {
		t.Fatal("unavailable auth reached publisher")
	}
	if !reflect.DeepEqual(before, legacyState()) {
		t.Fatal("modern API bound legacy globals")
	}
	if err := s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.redis.closed.Load() != 0 || h.sql.closed.Load() != 0 {
		t.Fatal("drain closed clients")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	apiWait(t, s.Done())
	if h.redis.closed.Load() != 1 || h.sql.closed.Load() != 1 || s.AcceptanceReady() || s.AuthenticationReady() {
		t.Fatal("API cleanup/health incomplete")
	}
}

func TestAPIStopAdmissionPreservesAdmittedPublishForDrain(t *testing.T) {
	h := newAPIHarness()
	entered, release := make(chan struct{}), make(chan struct{})
	var admitted context.Context
	h.publisher.publish = func(ctx context.Context, c messaging.Command, at time.Time) (rabbitmq.PublishResult, error) {
		admitted = ctx
		close(entered)
		<-release
		e, _ := messaging.NewEnvelope(c, at)
		return rabbitmq.PublishResult{Outcome: rabbitmq.PublishAccepted, CommandID: c.CommandID, PublishAttemptID: e.PublishAttemptID, ConnectionEpoch: 1, ChannelEpoch: 1, DeliveryTag: 1}, nil
	}
	s := h.service(t)
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	if err := s.Start(context.Background(), context.Background()); err != nil {
		t.Fatal(err)
	}
	token := apiToken(t, h)
	address := apiAddress(s)
	reply := make(chan int, 1)
	go func() { code, _ := apiSend(t, address, token); reply <- code }()
	apiWait(t, entered)
	s.StopAdmission()
	if admitted.Err() != nil || h.redis.closed.Load() != 0 || s.AcceptanceReady() {
		t.Fatal("stop canceled admitted work or closed auth")
	}
	if code, body := apiSend(t, address, token); code != 503 || !strings.Contains(body, "not_published") {
		t.Fatal("stopped admission did not reject new send", code, body)
	}
	drained := make(chan error, 1)
	go func() { drained <- s.Drain(context.Background()) }()
	unblock()
	if code := <-reply; code != 202 {
		t.Fatal("drain lost definite confirmation", code)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if h.redis.closed.Load() != 0 {
		t.Fatal("HTTP drain released auth storage")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type apiObservedMonitor struct {
	apiMonitor
	stopped chan context.Context
}

func (m *apiObservedMonitor) StopContext(ctx context.Context) error {
	select {
	case m.stopped <- ctx:
	default:
	}
	return m.apiMonitor.StopContext(ctx)
}
func TestAPIExpiredCloseRetainsClientsUntilRequestsAndProbesJoin(t *testing.T) {
	h := newAPIHarness()
	h.o.CloseTimeout = 30 * time.Millisecond
	requestEntered, releaseRequest := make(chan struct{}), make(chan struct{})
	probeEntered, releaseProbe := make(chan struct{}), make(chan struct{})
	var probeOnce sync.Once
	h.sql.probe = func(context.Context) error { probeOnce.Do(func() { close(probeEntered) }); <-releaseProbe; return nil }
	h.publisher.publish = func(ctx context.Context, c messaging.Command, at time.Time) (rabbitmq.PublishResult, error) {
		close(requestEntered)
		<-releaseRequest
		return rabbitmq.PublishResult{Outcome: rabbitmq.PublishUnknown}, ctx.Err()
	}
	stopping := make(chan context.Context, 2)
	original := h.d.monitor
	h.d.monitor = func(f func(context.Context) error, i, d time.Duration) apiMonitor {
		return &apiObservedMonitor{original(f, i, d), stopping}
	}
	s := h.service(t)
	unblockRequest := sync.OnceFunc(func() { close(releaseRequest) })
	unblockProbe := sync.OnceFunc(func() { close(releaseProbe) })
	t.Cleanup(unblockRequest)
	t.Cleanup(unblockProbe)
	if err := s.Start(context.Background(), context.Background()); err != nil {
		t.Fatal(err)
	}
	apiWait(t, probeEntered)
	token := apiToken(t, h)
	reply := make(chan struct{})
	address := apiAddress(s)
	go func() {
		defer close(reply)
		request, _ := http.NewRequest("POST", "http://"+address+"/auth/sendMessage", strings.NewReader(apiTestBody))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := (&http.Client{Timeout: time.Second}).Do(request)
		if err == nil {
			response.Body.Close()
		}
	}()
	apiWait(t, requestEntered)
	observer, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(observer); !errors.Is(err, errAPIPending) || !errors.Is(err, context.Canceled) {
		t.Fatal("expired close did not preserve pending ownership", err)
	}
	phase := <-stopping
	apiWait(t, phase.Done())
	if h.redis.closed.Load() != 0 || h.sql.closed.Load() != 0 {
		t.Fatal("close deadline was mistaken for task/probe completion")
	}
	unblockRequest()
	apiWait(t, reply)
	if h.sql.closed.Load() != 0 {
		t.Fatal("request exit discarded remaining probe")
	}
	unblockProbe()
	apiWait(t, s.Done())
	if h.redis.closed.Load() != 1 || h.sql.closed.Load() != 1 || !errors.Is(s.Err(), context.DeadlineExceeded) {
		t.Fatal("eventual cleanup lost ownership or error", s.Err())
	}
}

func TestAPIStartupRollbackOwnsEveryPartialHandle(t *testing.T) {
	for _, stage := range []string{"sql_error", "sql_nil", "redis_error", "redis_nil", "tokens_error", "auth_nil", "storage_nil", "status_error", "status_nil", "status_typed_nil", "conversations_error", "conversations_nil", "conversations_typed_nil", "sync_error", "sync_nil", "sync_typed_nil", "chat_error", "chat_nil", "http_error", "http_nil", "listener_error", "listener_nil", "handoff_cancel"} {
		t.Run(stage, func(t *testing.T) {
			h := newAPIHarness()
			cause := errors.New("PRIVATE_START_CAUSE")
			sqlOwned, redisOwned := false, false
			var listener net.Listener
			baseSQL, baseRedis, baseTokens, baseMonitor, baseHTTP, baseListen := h.d.newSQL, h.d.newRedis, h.d.tokens, h.d.monitor, h.d.http, h.d.listen
			h.d.newSQL = func(c config.Config) (apiSQL, error) {
				if stage == "sql_nil" {
					return nil, nil
				}
				sqlOwned = true
				r, e := baseSQL(c)
				if stage == "sql_error" {
					e = cause
				}
				return r, e
			}
			h.d.newRedis = func(c config.Config, o apiOptions) (apiRedis, error) {
				if stage == "redis_nil" {
					var nilRedis *apiFakeRedis
					return nilRedis, nil
				}
				redisOwned = true
				r, e := baseRedis(c, o)
				if stage == "redis_error" {
					e = cause
				}
				return r, e
			}
			h.d.tokens = func(c config.Config, r apiRedis) (*utils.TokenService, error) {
				if stage == "tokens_error" {
					return nil, cause
				}
				return baseTokens(c, r)
			}
			if stage == "chat_error" {
				h.d.chat = func(apiSQL, apiRedis) (*service.ReliableChat, error) { return nil, cause }
			}
			if strings.HasPrefix(stage, "status_") {
				h.d.status = func(apiSQL) (service.CommandStatusReader, error) {
					if stage == "status_error" {
						return nil, cause
					}
					if stage == "status_typed_nil" {
						return apiStatusReaderFunc(nil), nil
					}
					return nil, nil
				}
			}
			if stage == "chat_nil" {
				h.d.chat = func(apiSQL, apiRedis) (*service.ReliableChat, error) { return nil, nil }
			}
			if strings.HasPrefix(stage, "conversations_") {
				h.d.conversations = func(apiSQL) (service.ConversationReader, error) {
					if stage == "conversations_error" {
						return nil, cause
					}
					if stage == "conversations_typed_nil" {
						return apiConversationsReaderFunc(nil), nil
					}
					return nil, nil
				}
			}
			probes := 0
			if strings.HasPrefix(stage, "sync_") {
				h.d.syncMessages = func(apiSQL) (service.MessageSyncReader, error) {
					if stage == "sync_error" {
						return nil, cause
					}
					if stage == "sync_typed_nil" {
						return apiSyncReaderFunc(nil), nil
					}
					return nil, nil
				}
			}
			h.d.monitor = func(f func(context.Context) error, i, d time.Duration) apiMonitor {
				probes++
				if (stage == "auth_nil" && probes == 1) || (stage == "storage_nil" && probes == 2) {
					var nilProbe *health.Storage
					return nilProbe
				}
				return baseMonitor(f, i, d)
			}
			h.d.http = func(handler http.Handler, o apiOptions) (*legacyHTTPServer, error) {
				if stage == "http_nil" {
					return nil, nil
				}
				r, e := baseHTTP(handler, o)
				if stage == "http_error" {
					e = cause
				}
				return r, e
			}
			setup, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.d.listen = func(ctx context.Context, address string) (net.Listener, error) {
				if stage == "listener_nil" {
					var l *net.TCPListener
					return l, nil
				}
				var e error
				listener, e = baseListen(ctx, address)
				if stage == "listener_error" {
					e = cause
				}
				if stage == "handoff_cancel" {
					cancel()
				}
				return listener, e
			}
			s := h.service(t)
			if err := s.Start(setup, context.Background()); err == nil {
				t.Fatal("broken startup succeeded")
			}
			if err := s.Close(context.Background()); err == nil || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "PRIVATE") {
				t.Fatal("startup failure not preserved/redacted", err)
			}
			apiWait(t, s.Done())
			if (sqlOwned && h.sql.closed.Load() != 1) || (!sqlOwned && h.sql.closed.Load() != 0) || (redisOwned && h.redis.closed.Load() != 1) || (!redisOwned && h.redis.closed.Load() != 0) {
				t.Fatal("partial startup dropped or invented resource ownership")
			}
			if listener != nil {
				if connection, err := net.DialTimeout("tcp", listener.Addr().String(), 50*time.Millisecond); err == nil {
					connection.Close()
					t.Fatal("idle listener leaked")
				}
			}
		})
	}
}

func TestAPISlowHeadersAndBodyAreBoundedByServer(t *testing.T) {
	h := newAPIHarness()
	s := h.service(t)
	if err := s.Start(context.Background(), context.Background()); err != nil {
		t.Fatal(err)
	}
	address := apiAddress(s)
	token := apiToken(t, h)
	for _, part := range []string{"header", "body"} {
		t.Run(part, func(t *testing.T) {
			connection, err := net.DialTimeout("tcp", address, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			connection.SetDeadline(time.Now().Add(time.Second))
			if part == "header" {
				io.WriteString(connection, "POST /auth/sendMessage HTTP/1.1\r\nHost: api\r\nX-Unfinished: ")
			} else {
				fmt.Fprintf(connection, "POST /auth/sendMessage HTTP/1.1\r\nHost: api\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n{", token, len(apiTestBody))
			}
			started := time.Now()
			response, err := http.ReadResponse(bufio.NewReader(connection), nil)
			if err == nil {
				response.Body.Close()
				if response.StatusCode == 202 {
					t.Fatal("incomplete request accepted")
				}
			}
			if time.Since(started) > 700*time.Millisecond {
				t.Fatal("slow request survived ingress deadline")
			}
		})
	}
	if h.publisher.calls.Load() != 0 {
		t.Fatal("incomplete body published")
	}
	response, err := http.Get("http://" + address + "/health/live")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("timed-out peers broke liveness")
	}
}

func TestAPIUnexpectedListenerExitAndIdleClose(t *testing.T) {
	h := newAPIHarness()
	s := h.service(t)
	if err := s.Start(context.Background(), context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	listener := s.resources.listener
	s.mu.Unlock()
	listener.Close()
	apiWait(t, s.Done())
	if !errors.Is(s.Err(), errAPIStopped) || h.redis.closed.Load() != 1 || h.sql.closed.Load() != 1 {
		t.Fatal("unexpected exit did not retire service", s.Err())
	}
	idle := h.service(t)
	if err := idle.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	apiWait(t, idle.Done())
	if err := idle.Start(context.Background(), context.Background()); err == nil {
		t.Fatal("closed idle service restarted")
	}
}

func TestAPIValidationPrecedesActivation(t *testing.T) {
	for _, name := range []string{"mode", "address", "header", "body", "write", "idle", "submission", "status_timeout", "status_write", "status_factory", "conversations_timeout", "conversations_write", "conversations_factory", "sync_timeout", "sync_write", "sync_factory", "probe", "close", "headers", "jwt", "redis", "redis_pool", "mysql", "dependency", "publisher", "health", "consumer"} {
		t.Run(name, func(t *testing.T) {
			h := newAPIHarness()
			inputs := application.RoleDependencies{Publisher: h.publisher, Health: apiTestHealth{}}
			switch name {
			case "mode":
				h.c.RUNTIME.Mode = config.ModeWorker
			case "address":
				h.o.Address = "127.0.0.1:65536"
			case "header":
				h.o.ReadHeaderTimeout = h.o.ReadTimeout + 1
			case "body":
				h.o.ReadTimeout = 0
			case "write":
				h.o.WriteTimeout = h.o.SubmissionTimeout
			case "idle":
				h.o.IdleTimeout = 0
			case "submission":
				h.o.SubmissionTimeout = time.Minute + 1
			case "status_timeout":
				h.o.StatusTimeout = 0
			case "status_write":
				h.o.StatusTimeout = h.o.WriteTimeout
			case "status_factory":
				h.d.status = nil
			case "conversations_timeout":
				h.o.ConversationsTimeout = 0
			case "conversations_write":
				h.o.ConversationsTimeout = h.o.WriteTimeout
			case "conversations_factory":
				h.d.conversations = nil
			case "sync_timeout":
				h.o.SyncTimeout = 0
			case "sync_write":
				h.o.SyncTimeout = h.o.WriteTimeout
			case "sync_factory":
				h.d.syncMessages = nil
			case "probe":
				h.o.ProbeTimeout = 0
			case "close":
				h.o.CloseTimeout = 0
			case "headers":
				h.o.MaxHeaderBytes = 0
			case "jwt":
				h.c.JWT.Key = ""
			case "redis":
				h.c.REDIS.Addr = "127.0.0.1:0"
			case "redis_pool":
				h.c.REDIS.PoolSize = 0
			case "mysql":
				h.c.MYSQL.Port = "0"
			case "dependency":
				h.d.listen = nil
			case "publisher":
				var p *apiPublisher
				inputs.Publisher = p
			case "health":
				inputs.Health = nil
			case "consumer":
				inputs.Consumers = make([]application.PreparedConsumer, 1)
			}
			h.d.newSQL = func(config.Config) (apiSQL, error) { t.Fatal("validation opened SQL"); return nil, nil }
			if _, err := newAPIService(h.c, inputs, h.o, h.d); !errors.Is(err, errAPIInput) {
				t.Fatal("invalid API accepted", err)
			}
		})
	}
}

func TestAPICloseDuringStartupRetainsPartialResources(t *testing.T) {
	h := newAPIHarness()
	entered := make(chan struct{})
	base := h.d.listen
	h.d.listen = func(ctx context.Context, address string) (net.Listener, error) {
		l, err := base(ctx, address)
		close(entered)
		<-ctx.Done()
		return l, err
	}
	s := h.service(t)
	started := make(chan error, 1)
	go func() { started <- s.Start(context.Background(), context.Background()) }()
	apiWait(t, entered)
	if err := s.Close(context.Background()); !errors.Is(err, errAPIStart) {
		t.Fatal("startup close lost rollback error", err)
	}
	if err := <-started; !errors.Is(err, context.Canceled) {
		t.Fatal("startup did not observe stop", err)
	}
	if h.redis.closed.Load() != 1 || h.sql.closed.Load() != 1 {
		t.Fatal("startup stop leaked partial clients")
	}
}
