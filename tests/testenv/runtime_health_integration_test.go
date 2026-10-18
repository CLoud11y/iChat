//go:build integration

package testenv

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"iChat/application"
	"iChat/config"
	"iChat/health"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/router"
	"iChat/utils"
)

// These explicitly declared fixture capabilities exercise health composition.
// They are not production accepted handlers, business consumers or Redis JWT
// checks. Real subscriptions stay idle on this fixture's empty queues.
type readinessFixtureService struct {
	role      application.RuntimeRole
	inputs    application.RoleDependencies
	auth, sql *health.Storage
	admitting atomic.Bool
	done      chan struct{}
	once      sync.Once
	channels  []*rabbitmq.ConsumerChannel
	server    *httptest.Server
	setup     context.Context
	lifetime  context.Context
	started   chan<- *readinessFixtureService
	draining  chan struct{}
	release   chan struct{}
}

func (s *readinessFixtureService) Start(setup, lifetime context.Context) error {
	s.setup, s.lifetime = setup, lifetime
	for _, consumer := range s.inputs.Consumers {
		ch, err := consumer.Session.OpenConsumer(setup, consumer.Queue, consumer.Prefetch)
		if err != nil {
			return err
		}
		s.channels = append(s.channels, ch)
	}
	if s.role == application.RoleAPI {
		r := gin.New()
		router.RegisterHealth(r, s.sql, s.inputs.Health)
		s.server = httptest.NewServer(r)
	}
	s.admitting.Store(true)
	s.started <- s
	return nil
}
func (s *readinessFixtureService) AcceptanceReady() bool     { return s.admitting.Load() }
func (s *readinessFixtureService) AuthenticationReady() bool { return s.auth.StorageReady() }
func (s *readinessFixtureService) ConsumerReady() bool {
	if !s.admitting.Load() || !s.sql.StorageReady() || len(s.channels) == 0 {
		return false
	}
	for _, ch := range s.channels {
		select {
		case <-ch.Done():
			return false
		default:
		}
	}
	return true
}
func (s *readinessFixtureService) StopAdmission() { s.admitting.Store(false) }
func (s *readinessFixtureService) Drain(ctx context.Context) error {
	if s.draining == nil {
		return nil
	}
	close(s.draining)
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *readinessFixtureService) Close(ctx context.Context) error {
	var failures []error
	for _, ch := range s.channels {
		failures = append(failures, ch.Close(ctx))
	}
	if s.server != nil {
		s.server.Close()
	}
	s.once.Do(func() { close(s.done) })
	return errors.Join(failures...)
}
func (s *readinessFixtureService) Done() <-chan struct{} { return s.done }
func (*readinessFixtureService) Err() error              { return nil }

func TestRuntimeReadinessIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, broker := startRabbitFixture(t, ctx)
	management, err := rabbitmq.NewManagement("http://"+broker.ManagementAddress, rabbitAdminUser, env.password)
	if err != nil {
		t.Fatal(err)
	}
	control := openPublisherConnection(t, ctx, broker)
	ch, err := control.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, ch, management, broker.VHost); err != nil {
		t.Fatal(err)
	}
	c, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	c.RUNTIME.Mode = config.ModeAll
	c.RABBITMQ = config.RabbitMQConfig{
		URL: broker.AMQPURL(), VHost: broker.VHost,
		ManagementURL: "http://" + broker.ManagementAddress, ManagementUsername: rabbitAdminUser, ManagementPassword: env.password,
		PublisherSlots: 1, ConsumerWorkers: 1, ConsumerPrefetch: 1,
		PublishTimeout: time.Second, ConsumerOperationTimeout: time.Second,
		ConnectTimeout: 5 * time.Second, WriteTimeout: time.Second, CloseTimeout: time.Second, Heartbeat: time.Second,
		RetryMin: 50 * time.Millisecond, RetryMax: 200 * time.Millisecond,
	}
	db, err := utils.OpenMySQL(c)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	storage := health.StartStorage(pool.PingContext, 50*time.Millisecond, 200*time.Millisecond)
	t.Cleanup(storage.Stop)
	var redisClient atomic.Pointer[redis.Client]
	openRedis := func() {
		t.Helper()
		r, err := utils.OpenRedis(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		redisClient.Store(r)
		t.Cleanup(func() { r.Close() })
	}
	openRedis()
	auth := health.StartStorage(func(ctx context.Context) error { return redisClient.Load().Ping(ctx).Err() }, 50*time.Millisecond, 100*time.Millisecond)
	t.Cleanup(auth.Stop)
	waitFor := func(t *testing.T, condition func() bool) {
		t.Helper()
		wait, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for !condition() {
			select {
			case <-wait.Done():
				t.Fatal("runtime health condition did not become true")
			case <-ticker.C:
			}
		}
	}
	client := &http.Client{Timeout: time.Second}
	status := func(t *testing.T, api *readinessFixtureService, path string) (int, string) {
		t.Helper()
		response, err := client.Get(api.server.URL + path)
		if err != nil {
			t.Fatal("health HTTP request failed", err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if path != "/health/live" && response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("health response could be cached")
		}
		return response.StatusCode, string(body)
	}
	assertStatus := func(t *testing.T, api *readinessFixtureService, path string, want int, reason string) {
		t.Helper()
		code, body := status(t, api, path)
		if code != want || !strings.Contains(body, reason) {
			t.Fatal("incorrect observed readiness", path, code, body)
		}
	}
	start := func(t *testing.T, holdDrain bool) (map[application.RuntimeRole]*readinessFixtureService, context.CancelFunc, <-chan struct{}) {
		t.Helper()
		services := map[application.RuntimeRole]*readinessFixtureService{}
		var apiPointer atomic.Pointer[readinessFixtureService]
		started := make(chan *readinessFixtureService, 4)
		factory := func(role application.RuntimeRole) application.RuntimeFactory {
			return func(_ context.Context, inputs application.RoleDependencies) (application.RuntimeService, error) {
				s := &readinessFixtureService{role: role, inputs: inputs, auth: auth, sql: storage, done: make(chan struct{}), started: started}
				if holdDrain && role == application.RoleAPI {
					s.draining, s.release = make(chan struct{}), make(chan struct{})
				}
				services[role] = s
				if role == application.RoleAPI {
					apiPointer.Store(s)
				}
				return s, nil
			}
		}
		f := application.RuntimeFactories{API: factory(application.RoleAPI), Persistence: factory(application.RolePersistence), Quarantine: factory(application.RoleQuarantine), Outbox: factory(application.RoleOutbox)}
		process, stop := context.WithCancel(ctx)
		finished := make(chan struct{})
		var result error
		go func() {
			result = application.RunRuntime(process, c, f, application.DefaultRuntimeOptions())
			close(finished)
		}()
		t.Cleanup(func() {
			stop()
			if holdDrain {
				if api := apiPointer.Load(); api != nil && api.release != nil {
					close(api.release)
				}
			}
			select {
			case <-finished:
				if result != nil {
					t.Error("runtime readiness fixture shutdown failed", result)
				}
			case <-time.After(15 * time.Second):
				t.Error("runtime readiness fixture did not join")
			}
		})
		var last *readinessFixtureService
		for range 4 {
			select {
			case last = <-started:
			case <-finished:
				t.Fatal("runtime readiness fixture failed during activation", result)
			case <-time.After(30 * time.Second):
				t.Fatal("runtime readiness fixture activation timed out")
			}
		}
		<-last.setup.Done() // Publishes immutable role bindings and service fields.
		api := services[application.RoleAPI]
		waitFor(t, func() bool { code, _ := status(t, api, "/health/ready/accept"); return code == 200 })
		return services, stop, finished
	}
	t.Run("redis_auth_connection_failure_and_recovery", func(t *testing.T) {
		services, _, _ := start(t, false)
		api := services[application.RoleAPI]
		waitFor(t, func() bool { code, _ := status(t, api, "/health/ready/consumers"); return code == 200 })
		if err := redisClient.Load().Close(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { code, _ := status(t, api, "/health/ready/accept"); return code == 503 })
		assertStatus(t, api, "/health/ready/accept", 503, "AUTHENTICATION_UNAVAILABLE")
		assertStatus(t, api, "/health/ready/consumers", 200, "ready")
		assertStatus(t, api, "/health/live", 200, "live")
		openRedis()
		waitFor(t, func() bool { code, _ := status(t, api, "/health/ready/accept"); return code == 200 })
	})
	t.Run("publisher_topology_retirement_and_new_epoch", func(t *testing.T) {
		services, _, _ := start(t, false)
		api := services[application.RoleAPI]
		initial := api.inputs.Publisher.Status().ConnectionEpoch
		if err := ch.QueueUnbind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ch.QueueBind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil)
		})
		command, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "health-fixture:" + env.RunID()})
		if err != nil {
			t.Fatal(err)
		}
		published, err := api.inputs.Publisher.Publish(ctx, command, time.Now())
		if err == nil || (published.Outcome != rabbitmq.PublishUnroutable && published.Outcome != rabbitmq.PublishNotStarted) {
			t.Fatal("missing binding was accepted", published, err)
		}
		assertStatus(t, api, "/health/ready/accept", 503, "MQ_PUBLISHER_UNAVAILABLE")
		assertStatus(t, api, "/health/ready/consumers", 200, "ready")
		if err := ch.QueueBind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { code, _ := status(t, api, "/health/ready/accept"); return code == 200 })
		if api.inputs.Publisher.Status().ConnectionEpoch <= initial {
			t.Fatal("readiness recovery reused retired epoch")
		}
	})
	t.Run("consumer_connection_fault_does_not_block_acceptance", func(t *testing.T) {
		services, _, _ := start(t, false)
		api := services[application.RoleAPI]
		waitFor(t, func() bool { code, _ := status(t, api, "/health/ready/consumers"); return code == 200 })
		if err := services[application.RolePersistence].channels[0].Close(ctx); err != nil {
			t.Fatal(err)
		}
		assertStatus(t, api, "/health/ready/consumers", 503, "MQ_CONSUMER_UNAVAILABLE")
		assertStatus(t, api, "/health/ready/accept", 200, "ready")
	})
	t.Run("mysql_offline_accepts_until_process_drain", func(t *testing.T) {
		services, stop, _ := start(t, true)
		api := services[application.RoleAPI]
		waitFor(t, func() bool { code, _ := status(t, api, "/health/ready/consumers"); return code == 200 })
		if err := env.SetMySQLRunning(ctx, false); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { code, _ := status(t, api, "/health/ready/storage"); return code == 503 })
		assertStatus(t, api, "/health/ready/consumers", 503, "MQ_CONSUMER_UNAVAILABLE")
		assertStatus(t, api, "/health/ready/accept", 200, "ready")
		assertStatus(t, api, "/health/live", 200, "live")
		stop()
		select {
		case <-api.draining:
		case <-time.After(time.Second):
			t.Fatal("process did not enter drain")
		}
		assertStatus(t, api, "/health/ready/accept", 503, "RUNTIME_STOPPING")
		assertStatus(t, api, "/health/ready/consumers", 503, "RUNTIME_STOPPING")
		if api.lifetime.Err() != nil || !api.inputs.Publisher.Status().Ready {
			t.Fatal("health withdrawal canceled MQ/role lifetime before drain")
		}
	})
}
