//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"

	"iChat/application"
	"iChat/config"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/tests/testenv"
	"iChat/utils"

	amqp "github.com/rabbitmq/amqp091-go"
)

// The barrier holds a real, already confirmed receipt; it never manufactures
// acceptance or substitutes a fixture service for the production API role.
type apiReceiptBarrier struct {
	application.MQPublisher
	entered, release chan struct{}
	ctx              context.Context // published through entered
}

func (b *apiReceiptBarrier) Publish(ctx context.Context, c messaging.Command, at time.Time) (rabbitmq.PublishResult, error) {
	r, err := b.MQPublisher.Publish(ctx, c, at)
	if err == nil && r.Outcome == rabbitmq.PublishAccepted {
		b.ctx = ctx
		close(b.entered)
		<-b.release
	}
	return r, err
}

type apiRealProcess struct {
	s            *apiService
	cancel       context.CancelFunc
	result       chan error
	unregistered atomic.Bool
	wantError    error // Set by the scenario before its cleanup joins the executor.
}

func startRealAPIProcess(t *testing.T, c config.Config, o apiOptions, runtimeOptions application.RuntimeOptions, address string, barrier *apiReceiptBarrier) *apiRealProcess {
	t.Helper()
	r := &apiRealProcess{result: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	d := defaultProcessDependencies()
	d.legacy.loadConfig = func() (config.Config, error) { return c, nil }
	d.api.options = func(config.Config) apiOptions { return o }
	d.options = runtimeOptions
	if address != "" {
		d.api.dependencies.listen = func(ctx context.Context, _ string) (net.Listener, error) {
			return (&net.ListenConfig{}).Listen(ctx, "tcp", address)
		}
	}
	d.notifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
		return ctx, func() { r.unregistered.Store(true); cancel() }
	}
	constructed := make(chan *apiService, 1)
	d.runRuntime = func(ctx context.Context, c config.Config, f application.RuntimeFactories, o application.RuntimeOptions) error {
		factory := f.API
		f.API = func(ctx context.Context, inputs application.RoleDependencies) (application.RuntimeService, error) {
			if barrier != nil {
				barrier.MQPublisher = inputs.Publisher
				inputs.Publisher = barrier
			}
			s, err := factory(ctx, inputs)
			if s != nil {
				constructed <- s.(*apiService)
			}
			return s, err
		}
		return application.RunRuntime(ctx, c, f, o)
	}
	go func() { r.result <- runProcess(d) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-r.result:
			if (r.wantError == nil && err != nil) || (r.wantError != nil && !errors.Is(err, r.wantError)) {
				t.Error("unexpected production process result", err)
			}
			if !r.unregistered.Load() {
				t.Error("production process did not release signal registration")
			}
		case <-time.After(5 * time.Second):
			t.Error("production runtime did not return")
		}
	})
	select {
	case r.s = <-constructed:
	case err := <-r.result:
		t.Fatal("production API not constructed", err)
	case <-time.After(20 * time.Second):
		t.Fatal("production API preparation timed out")
	}
	select {
	case <-r.s.prepared:
	case <-time.After(15 * time.Second):
		t.Fatal("production API startup did not finish")
	}
	return r
}

func apiRealEventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("production API state did not converge")
}
func apiRealStatus(address, path string) int {
	r, err := (&http.Client{Timeout: time.Second}).Get("http://" + address + path)
	if err != nil {
		return 0
	}
	defer r.Body.Close()
	io.Copy(io.Discard, r.Body)
	return r.StatusCode
}

type apiRealResponse struct {
	status int
	body   messaging.SubmissionResponse
	err    error
}

func apiRealSend(address, token string) apiRealResponse {
	request, _ := http.NewRequest("POST", "http://"+address+"/auth/sendMessage", strings.NewReader(apiTestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	r, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return apiRealResponse{err: err}
	}
	defer r.Body.Close()
	result := apiRealResponse{status: r.StatusCode}
	result.err = json.NewDecoder(r.Body).Decode(&result.body)
	return result
}
func apiRealClosed(t *testing.T, s *apiService) {
	t.Helper()
	apiWait(t, s.Done())
	s.mu.Lock()
	resources := s.resources
	s.mu.Unlock()
	if resources.auth != nil {
		apiWait(t, resources.auth.Done())
	}
	if resources.storage != nil {
		apiWait(t, resources.storage.Done())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if resources.redis != nil && !errors.Is(resources.redis.Get(ctx, "closed-probe").Err(), redis.ErrClosed) {
		t.Fatal("Redis still open after API completion")
	}
	if resources.sql != nil {
		if err := resources.sql.PingContext(ctx); err == nil || err.Error() != "sql: database is closed" {
			t.Fatal("SQL pool not closed after API completion", err)
		}
	}
}

// Only loopback connections to this run's Redis are interrupted. All proxy
// connection tasks are joined before the fixture can remove its resources.
type apiRedisProxy struct {
	mu          sync.Mutex
	listener    net.Listener
	upstream    string
	blocked     bool
	connections map[net.Conn]bool
	wg          sync.WaitGroup
}

func newAPIRedisProxy(t *testing.T, upstream string) *apiRedisProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &apiRedisProxy{listener: l, upstream: upstream, connections: map[net.Conn]bool{}, blocked: true}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			p.wg.Add(1)
			go p.forward(client)
		}
	}()
	t.Cleanup(func() { p.setBlocked(true); l.Close(); p.wg.Wait() })
	return p
}
func (p *apiRedisProxy) setBlocked(blocked bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked = blocked
	if blocked {
		for c := range p.connections {
			c.Close()
		}
	}
}
func (p *apiRedisProxy) forward(client net.Conn) {
	defer p.wg.Done()
	defer client.Close()
	p.mu.Lock()
	if p.blocked {
		p.mu.Unlock()
		return
	}
	p.connections[client] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, client); p.mu.Unlock() }()
	upstream, err := net.DialTimeout("tcp", p.upstream, time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	p.mu.Lock()
	if p.blocked {
		p.mu.Unlock()
		return
	}
	p.connections[upstream] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, upstream); p.mu.Unlock() }()
	joined := make(chan struct{})
	go func() { defer close(joined); io.Copy(upstream, client); upstream.Close() }()
	io.Copy(client, upstream)
	client.Close()
	<-joined
}

func TestProductionAPIIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.StartWithRabbitMQ(ctx, "docker-compose.test.yml")
	if env != nil {
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := env.Close(cleanup); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	c, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	broker, err := env.RabbitMQ()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("owned fixture runId=%s RabbitMQ=%s", env.RunID(), rabbitmq.BrokerVersion)
	c.RUNTIME.Mode = config.ModeAPI
	c.LEGACY_WORKER.Enabled = false
	c.JWT.Key = "production-api-" + env.RunID()
	c.JWT.TokenHourLifeSpan = 1
	c.RABBITMQ = config.RabbitMQConfig{URL: broker.AMQPURL(), VHost: broker.VHost, ManagementURL: "http://" + broker.ManagementAddress, ManagementUsername: "ichat_test_admin", ManagementPassword: c.REDIS.Password,
		PublisherSlots: 1, ConsumerWorkers: 1, ConsumerPrefetch: 1, PublishTimeout: 2 * time.Second, ConsumerOperationTimeout: 2 * time.Second, ConnectTimeout: 5 * time.Second, WriteTimeout: time.Second, CloseTimeout: time.Second, Heartbeat: time.Second, RetryMin: 50 * time.Millisecond, RetryMax: 200 * time.Millisecond}
	m, err := rabbitmq.NewManagement(c.RABBITMQ.ManagementURL, c.RABBITMQ.ManagementUsername, c.RABBITMQ.ManagementPassword)
	if err != nil {
		t.Fatal(err)
	}
	control, err := amqp.DialConfig(broker.AMQPURL(), amqp.Config{Heartbeat: time.Second, Dial: func(network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
		if err == nil {
			deadline, _ := ctx.Deadline()
			conn.SetDeadline(deadline)
		}
		return conn, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	ch, err := control.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, ch, m, broker.VHost); err != nil {
		t.Fatal(err)
	}
	tokens, err := utils.NewTokenService(c, utils.NewRedisTokenBlacklist(nil))
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokens.GenerateToken(42)
	if err != nil {
		t.Fatal(err)
	}
	o := defaultAPIOptions(c)
	o.Address = "127.0.0.1:0"
	o.ProbeInterval = 30 * time.Millisecond
	o.ProbeTimeout = 150 * time.Millisecond
	o.CloseTimeout = 300 * time.Millisecond
	ro := application.DefaultRuntimeOptions()
	ro.DrainTimeout = 2 * time.Second
	ro.CloseTimeout = 2 * time.Second
	before := legacyState()
	readQueued := func(t *testing.T, response apiRealResponse) {
		t.Helper()
		if response.err != nil || response.status != 202 || response.body.Data.State != "accepted" {
			t.Fatal("actual submission not accepted", response.status, response.err)
		}
		delivery, ok, err := ch.Get(rabbitmq.PersistQueue, false)
		if err != nil || !ok {
			t.Fatal("confirmed command not queued", err)
		}
		var envelope messaging.Envelope
		if err := json.Unmarshal(delivery.Body, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.CommandID != response.body.Data.CommandID || envelope.PublishAttemptID != response.body.Data.PublishAttemptID {
			t.Fatal("HTTP receipt did not match broker envelope")
		}
		// Fixture control ACK only; no business consumer/transaction is active.
		if err := delivery.Ack(false); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("redis_offline_start_and_recovery", func(t *testing.T) {
		proxy := newAPIRedisProxy(t, c.REDIS.Addr)
		conf := c
		conf.REDIS.Addr = proxy.listener.Addr().String()
		r := startRealAPIProcess(t, conf, o, ro, "", nil)
		address := apiAddress(r.s)
		if apiRealStatus(address, "/health/live") != 200 || apiRealStatus(address, "/health/ready/accept") != 503 {
			t.Fatal("offline auth prevented live startup or declared ready")
		}
		if response := apiRealSend(address, token); response.status != 503 {
			t.Fatal("offline auth accepted", response.status, response.err)
		}
		proxy.setBlocked(false)
		apiRealEventually(t, func() bool { return apiRealStatus(address, "/health/ready/accept") == 200 })
		readQueued(t, apiRealSend(address, token))
		proxy.setBlocked(true)
		apiRealEventually(t, func() bool { return apiRealStatus(address, "/health/ready/accept") == 503 })
		if response := apiRealSend(address, token); response.status != 503 {
			t.Fatal("broken auth accepted", response.status, response.err)
		}
		proxy.setBlocked(false)
		apiRealEventually(t, func() bool { return apiRealStatus(address, "/health/ready/accept") == 200 })
		readQueued(t, apiRealSend(address, token))
		if apiRealStatus(address, "/health/ready/consumers") != 503 || apiRealStatus(address, "/user/login") != 404 {
			t.Fatal("API exposed missing consumer/legacy role")
		}
		r.cancel()
		apiRealClosed(t, r.s)
	})
	for _, forced := range []bool{false, true} {
		name := "confirmed_receipt_drain"
		if forced {
			name = "forced_close_retains_request_ownership"
		}
		t.Run(name, func(t *testing.T) {
			barrier := &apiReceiptBarrier{entered: make(chan struct{}), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(barrier.release) })
			settings := ro
			options := o
			if forced {
				settings.DrainTimeout = 40 * time.Millisecond
				settings.CloseTimeout = 80 * time.Millisecond
				options.CloseTimeout = 30 * time.Millisecond
			}
			r := startRealAPIProcess(t, c, options, settings, "", barrier)
			if forced {
				r.wantError = application.ErrRuntimeShutdown
			}
			t.Cleanup(release)
			address := apiAddress(r.s)
			apiRealEventually(t, func() bool { return apiRealStatus(address, "/health/ready/accept") == 200 })
			reply := make(chan apiRealResponse, 1)
			go func() { reply <- apiRealSend(address, token) }()
			apiWait(t, barrier.entered)
			r.cancel()
			apiRealEventually(t, func() bool { return !r.s.AcceptanceReady() })
			if !forced {
				if barrier.ctx.Err() != nil {
					t.Fatal("process cancellation canceled admitted request before drain")
				}
				if r.unregistered.Load() {
					t.Fatal("signals unregistered before graceful completion")
				}
				release()
				response := <-reply
				readQueued(t, response)
				apiRealClosed(t, r.s)
			} else {
				select {
				case err := <-r.result:
					if !errors.Is(err, application.ErrRuntimeShutdown) {
						t.Fatal("forced stop lost shutdown failure", err)
					}
					r.result <- err
				case <-time.After(3 * time.Second):
					t.Fatal("forced runtime stop exceeded observer budget")
				}
				select {
				case <-r.s.Done():
					t.Fatal("pending publisher discarded API resources")
				default:
				}
				if err := r.s.resources.redis.Get(ctx, "retained-auth-probe").Err(); err != nil && !errors.Is(err, redis.Nil) {
					t.Fatal("pending handler lost Redis client")
				}
				if err := r.s.resources.sql.PingContext(ctx); err != nil {
					t.Fatal("pending handler lost SQL pool", err)
				}
				release()
				<-reply
				apiRealClosed(t, r.s)
				if !errors.Is(r.s.Err(), context.DeadlineExceeded) {
					t.Fatal("eventual API cleanup lost timeout", r.s.Err())
				}
				delivery, ok, err := ch.Get(rabbitmq.PersistQueue, false)
				if err != nil || !ok {
					t.Fatal("forced close lost already confirmed command", err)
				}
				delivery.Ack(false)
			}
		})
	}
	t.Run("occupied_listener_rolls_back", func(t *testing.T) {
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer occupied.Close()
		r := startRealAPIProcess(t, c, o, ro, occupied.Addr().String(), nil)
		r.wantError = application.ErrRuntimeStart
		apiRealClosed(t, r.s)
		if !errors.Is(r.s.Err(), errAPIStart) {
			t.Fatal("listener failure not preserved", r.s.Err())
		}
	})
	t.Run("mysql_offline_does_not_block_acceptance", func(t *testing.T) {
		if err := env.SetMySQLRunning(ctx, false); err != nil {
			t.Fatal(err)
		}
		r := startRealAPIProcess(t, c, o, ro, "", nil)
		address := apiAddress(r.s)
		apiRealEventually(t, func() bool { return apiRealStatus(address, "/health/ready/accept") == 200 })
		if apiRealStatus(address, "/health/live") != 200 || apiRealStatus(address, "/health/ready/storage") != 503 {
			t.Fatal("storage health not independent")
		}
		readQueued(t, apiRealSend(address, token))
		r.cancel()
		apiRealClosed(t, r.s)
	})
	if !reflect.DeepEqual(before, legacyState()) {
		t.Fatal("modern production API changed legacy globals")
	}
}
