package application

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/config"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

func mqTestConfig(mode config.RuntimeMode) config.Config {
	return config.Config{
		RUNTIME: config.RuntimeConfig{Mode: mode},
		RABBITMQ: config.RabbitMQConfig{
			URL: "amqp://app:secret@127.0.0.1:9/%2Fichat", VHost: "/ichat",
			ManagementURL: "http://127.0.0.1:9", ManagementUsername: "admin", ManagementPassword: "secret",
			PublisherSlots: 2, ConsumerWorkers: 3, ConsumerPrefetch: 7,
			PublishTimeout: time.Second, ConsumerOperationTimeout: 2 * time.Second,
			ConnectTimeout: time.Second, WriteTimeout: time.Second, CloseTimeout: time.Second, Heartbeat: time.Second,
			RetryMin: 10 * time.Millisecond, RetryMax: 20 * time.Millisecond,
		},
	}
}

type fakeMQResource struct {
	calls atomic.Int32
	close func(context.Context) error
	done  chan struct{}
	once  sync.Once
}

func (r *fakeMQResource) Close(ctx context.Context) error {
	r.calls.Add(1)
	r.once.Do(func() { close(r.done) })
	if r.close != nil {
		return r.close(ctx)
	}
	return nil
}

type fakeMQPublisher struct{ *fakeMQResource }

func (*fakeMQPublisher) Publish(context.Context, messaging.Command, time.Time) (rabbitmq.PublishResult, error) {
	return rabbitmq.PublishResult{}, errors.New("unexpected publish during preparation")
}
func (*fakeMQPublisher) WaitReady(context.Context) (uint64, error) {
	return 0, errors.New("unexpected readiness wait during preparation")
}
func (*fakeMQPublisher) Status() rabbitmq.SupervisorStatus { return rabbitmq.SupervisorStatus{} }

type fakeMQConsumer struct{ *fakeMQResource }

func (*fakeMQConsumer) OpenConsumer(context.Context, string, int) (*rabbitmq.ConsumerChannel, error) {
	panic("preparation must never create a subscription")
}
func (*fakeMQConsumer) Verify(context.Context) error { panic("factory already verifies connection") }
func (c *fakeMQConsumer) Done() <-chan struct{}      { return c.done }

func fakeMQFactories(handles *[]*fakeMQResource, life *context.Context) mqFactories {
	newHandle := func() *fakeMQResource {
		r := &fakeMQResource{done: make(chan struct{})}
		*handles = append(*handles, r)
		return r
	}
	return mqFactories{
		publisher: func(_ context.Context, ctx context.Context, _ config.PublisherConfig) (ownedPublisher, error) {
			*life = ctx
			return &fakeMQPublisher{newHandle()}, nil
		},
		consumer: func(context.Context, config.ConsumerConfig) (ownedConsumer, error) {
			return &fakeMQConsumer{newHandle()}, nil
		},
	}
}

func assertMQClosed(t *testing.T, handles []*fakeMQResource, calls int32) {
	t.Helper()
	for i, h := range handles {
		if h.calls.Load() != calls {
			t.Fatalf("handle %d close calls=%d, want %d", i, h.calls.Load(), calls)
		}
	}
}

func TestPrepareMQRolesAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		mode    config.RuntimeMode
		workers int
		pub     bool
		count   int
	}{
		{config.ModeLegacy, 3, false, 0}, {config.ModeAPI, 3, true, 0},
		{config.ModeWorker, 3, false, 4}, {config.ModeAll, 3, true, 4},
		{config.ModeWorker, config.MaxConsumerWorkers, false, config.MaxConsumerWorkers + 1},
	} {
		t.Run(fmt.Sprintf("%s_%d", tc.mode, tc.workers), func(t *testing.T) {
			c := mqTestConfig(tc.mode)
			c.RABBITMQ.ConsumerWorkers = tc.workers
			original, global := c, config.Conf
			var handles []*fakeMQResource
			var life context.Context
			f := fakeMQFactories(&handles, &life)
			if !tc.pub {
				f.publisher = nil // Unselected factories are not required.
			}
			if tc.count == 0 {
				f.consumer = nil
			}
			r, err := prepareMQ(context.Background(), c, DefaultMQOptions(), f)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			if r.Mode() != tc.mode || (r.Publisher() != nil) != tc.pub || len(r.Consumers()) != tc.count {
				t.Fatal("incorrect role capabilities")
			}
			for i, item := range r.Consumers() {
				queue := rabbitmq.PersistQueue
				if i == tc.count-1 {
					queue = rabbitmq.QuarantineQueue
				}
				if item.Queue != queue || item.Prefetch != 7 || item.Session == nil {
					t.Fatal("incorrect prepared consumer", i)
				}
			}
			if len(r.Consumers()) != 0 {
				copy := r.Consumers()
				copy[0] = PreparedConsumer{}
				if r.Consumers()[0].Session == nil || r.Consumers()[0].Queue != rabbitmq.PersistQueue {
					t.Fatal("caller changed internal resource list")
				}
			}
			if c != original || config.Conf != global {
				t.Fatal("configuration or legacy globals changed")
			}
			if err := r.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r.Publisher() != nil || len(r.Consumers()) != 0 || (life != nil && life.Err() == nil) {
				t.Fatal("closed resource capabilities or publisher lifetime remained active")
			}
			if err := r.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertMQClosed(t, handles, 1)
		})
	}
}

func TestPrepareMQRejectsBeforeFactories(t *testing.T) {
	for _, name := range []string{"nil_context", "cancelled", "zero_setup", "large_close", "mode", "conflict", "missing_mq", "bad_late_consumer", "missing_publisher_factory", "missing_consumer_factory"} {
		t.Run(name, func(t *testing.T) {
			c, o := mqTestConfig(config.ModeAll), DefaultMQOptions()
			ctx := context.Background()
			var handles []*fakeMQResource
			var life context.Context
			f := fakeMQFactories(&handles, &life)
			switch name {
			case "nil_context":
				ctx = nil
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "zero_setup":
				o.SetupTimeout = 0
			case "large_close":
				o.CloseTimeout = time.Minute + 1
			case "mode":
				c.RUNTIME.Mode = "secret-mode"
			case "conflict":
				c.LEGACY_WORKER.Enabled = true
			case "missing_mq":
				c.RABBITMQ = config.RabbitMQConfig{}
			case "bad_late_consumer":
				c.RABBITMQ.ConsumerWorkers = 0
			case "missing_publisher_factory":
				f.publisher = nil
			case "missing_consumer_factory":
				f.consumer = nil
			}
			if r, err := prepareMQ(ctx, c, o, f); err == nil || r != nil || len(handles) != 0 {
				t.Fatal("invalid input reached a factory/returned resources", r, err)
			}
		})
	}
	// Legacy ignores unused MQ settings without starting either factory.
	c := mqTestConfig(config.ModeLegacy)
	c.RABBITMQ = config.RabbitMQConfig{}
	r, err := PrepareMQ(context.Background(), c, DefaultMQOptions())
	if err != nil || len(r.Consumers()) != 0 || r.Publisher() != nil {
		t.Fatal("legacy acquired MQ dependencies", err)
	}
	_ = r.Close(context.Background())
}

func TestPrepareMQPartialFailureClosesEveryReturnedHandle(t *testing.T) {
	secretErr := errors.New("amqp://user:must-stay-secret@host/; raw payload")
	for failAt := 0; failAt < 5; failAt++ {
		t.Run(fmt.Sprintf("stage_%d", failAt), func(t *testing.T) {
			var handles []*fakeMQResource
			var life context.Context
			f := fakeMQFactories(&handles, &life)
			pub, consumer := f.publisher, f.consumer
			f.publisher = func(setup, lifetime context.Context, c config.PublisherConfig) (ownedPublisher, error) {
				r, err := pub(setup, lifetime, c)
				if failAt == 0 {
					return r, secretErr
				}
				return r, err
			}
			f.consumer = func(ctx context.Context, c config.ConsumerConfig) (ownedConsumer, error) {
				r, err := consumer(ctx, c)
				if len(handles)-1 == failAt {
					return r, secretErr
				}
				return r, err
			}
			r, err := prepareMQ(context.Background(), mqTestConfig(config.ModeAll), DefaultMQOptions(), f)
			if r != nil || !errors.Is(err, ErrMQPrepare) || !errors.Is(err, secretErr) || len(handles) != failAt+1 {
				t.Fatal("partial preparation escaped or lost error classification", err)
			}
			for _, format := range []string{"%v", "%+v", "%#v"} {
				if strings.Contains(fmt.Sprintf(format, err), "must-stay-secret") {
					t.Fatal("factory credentials escaped")
				}
			}
			assertMQClosed(t, handles, 1)
			if life.Err() == nil {
				t.Fatal("failed startup retained publisher recovery")
			}
		})
	}
}

func TestPrepareMQNilFactoryHandle(t *testing.T) {
	for _, typed := range []bool{false, true} {
		var handles []*fakeMQResource
		var life context.Context
		f := fakeMQFactories(&handles, &life)
		f.consumer = func(context.Context, config.ConsumerConfig) (ownedConsumer, error) {
			if typed {
				return (*fakeMQConsumer)(nil), nil
			}
			return nil, nil
		}
		if r, err := prepareMQ(context.Background(), mqTestConfig(config.ModeAll), DefaultMQOptions(), f); r != nil || !errors.Is(err, ErrMQPrepare) {
			t.Fatal("nil handle accepted", err)
		}
		assertMQClosed(t, handles, 1)
	}
}

func TestPrepareMQTotalSetupDeadlineAndIndependentRollback(t *testing.T) {
	var handles []*fakeMQResource
	var life context.Context
	f := fakeMQFactories(&handles, &life)
	consumer := f.consumer
	var deadlines []time.Time
	f.consumer = func(ctx context.Context, c config.ConsumerConfig) (ownedConsumer, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("missing total construction deadline")
		}
		deadlines = append(deadlines, deadline)
		select {
		case <-time.After(20 * time.Millisecond):
			r, err := consumer(ctx, c)
			handles[len(handles)-1].close = func(ctx context.Context) error {
				if ctx.Err() != nil {
					return errors.New("rollback incorrectly inherited cancelled setup")
				}
				return nil
			}
			return r, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	o := MQOptions{SetupTimeout: 55 * time.Millisecond, CloseTimeout: time.Second}
	start := time.Now()
	r, err := prepareMQ(context.Background(), mqTestConfig(config.ModeAll), o, f)
	if r != nil || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrMQClose) || time.Since(start) > time.Second {
		t.Fatal("unbounded setup or failed fresh rollback", err)
	}
	for _, d := range deadlines {
		if !d.Equal(deadlines[0]) {
			t.Fatal("deadline was reset per worker")
		}
	}
	assertMQClosed(t, handles, 1)
	if life.Err() == nil {
		t.Fatal("setup timeout retained publisher")
	}
}

func TestPrepareMQSetupContextOwnership(t *testing.T) {
	for _, cancelBeforeReturn := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		var handles []*fakeMQResource
		var life context.Context
		f := fakeMQFactories(&handles, &life)
		pub := f.publisher
		f.publisher = func(setup, lifetime context.Context, c config.PublisherConfig) (ownedPublisher, error) {
			r, err := pub(setup, lifetime, c)
			if cancelBeforeReturn {
				cancel() // A successful factory may race with setup cancellation.
			}
			return r, err
		}
		r, err := prepareMQ(ctx, mqTestConfig(config.ModeAPI), MQOptions{SetupTimeout: 30 * time.Millisecond, CloseTimeout: time.Second}, f)
		cancel()
		if cancelBeforeReturn {
			if r != nil || !errors.Is(err, context.Canceled) || life.Err() == nil {
				t.Fatal("cancelled setup transferred live ownership", err)
			}
		} else {
			if err != nil || life.Err() != nil {
				t.Fatal("successful setup inherited caller cancellation", err)
			}
			time.Sleep(50 * time.Millisecond)
			if life.Err() != nil {
				t.Fatal("setup timeout closed successfully transferred publisher")
			}
			_ = r.Close(context.Background())
		}
		assertMQClosed(t, handles, 1)
	}
}

func TestMQResourcesCloseTotalDeadlineConcurrentWaitAndRetry(t *testing.T) {
	var handles []*fakeMQResource
	var life context.Context
	r, err := prepareMQ(context.Background(), mqTestConfig(config.ModeAll), MQOptions{SetupTimeout: time.Second, CloseTimeout: 80 * time.Millisecond}, fakeMQFactories(&handles, &life))
	if err != nil {
		t.Fatal(err)
	}
	started, joined := make(chan struct{}, len(handles)), make(chan struct{}, len(handles))
	for _, h := range handles {
		h.close = func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			joined <- struct{}{}
			return ctx.Err()
		}
	}
	first := make(chan error, 1)
	start := time.Now()
	go func() { first <- r.Close(context.Background()) }()
	for range handles {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("shutdown did not start every resource together")
		}
	}
	wait, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Close(wait); !errors.Is(err, context.Canceled) {
		t.Fatal("concurrent closer ignored its shorter deadline", err)
	}
	if r.Publisher() != nil || len(r.Consumers()) != 0 || life.Err() == nil {
		t.Fatal("shutdown did not stop capability access/recovery")
	}
	if err := <-first; !errors.Is(err, ErrMQClose) || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("shutdown multiplied the deadline per resource", err)
	}
	if len(joined) != len(handles) {
		t.Fatal("shutdown abandoned a closer goroutine")
	}
	assertMQClosed(t, handles, 1)
	for _, h := range handles {
		h.close = nil
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal("incomplete shutdown could not be retried", err)
	}
	assertMQClosed(t, handles, 2)
}

func TestMQResourcesExpiredCloseReachesAllAndReportsRollbackErrors(t *testing.T) {
	var handles []*fakeMQResource
	var life context.Context
	f := fakeMQFactories(&handles, &life)
	r, err := prepareMQ(context.Background(), mqTestConfig(config.ModeAll), DefaultMQOptions(), f)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range handles {
		h.close = func(ctx context.Context) error { return ctx.Err() }
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Close(ctx); !errors.Is(err, ErrMQClose) || !errors.Is(err, context.Canceled) {
		t.Fatal("expired shutdown error lost", err)
	}
	assertMQClosed(t, handles, 1)
	for _, h := range handles {
		h.close = nil
	}
	_ = r.Close(context.Background())

	handles = nil
	f = fakeMQFactories(&handles, &life)
	pub := f.publisher
	setupErr, closeErr := errors.New("secret setup error"), errors.New("secret close error")
	f.publisher = func(setup, lifetime context.Context, c config.PublisherConfig) (ownedPublisher, error) {
		r, _ := pub(setup, lifetime, c)
		handles[0].close = func(context.Context) error { return closeErr }
		return r, setupErr
	}
	if r, err := prepareMQ(context.Background(), mqTestConfig(config.ModeAPI), DefaultMQOptions(), f); r != nil || !errors.Is(err, ErrMQPrepare) || !errors.Is(err, ErrMQClose) || !errors.Is(err, setupErr) || !errors.Is(err, closeErr) || strings.Contains(fmt.Sprintf("%#v", err), "secret") {
		t.Fatal("rollback error lost or leaked", err)
	}
	assertMQClosed(t, handles, 1)
}

func TestPrepareMQAPIOfflineStartsRecoveryWithoutWaitingForReady(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted, done := make(chan struct{}, 16), make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close() // Owned test peer rejects the AMQP handshake.
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	c := mqTestConfig(config.ModeAPI)
	c.RABBITMQ.URL = "amqp://app:secret@" + listener.Addr().String() + "/%2Fichat"
	c.RABBITMQ.ManagementURL = "http://" + listener.Addr().String()
	c.RABBITMQ.ConsumerWorkers = 0 // Unselected consumer settings need no construction.
	setup, cancel := context.WithCancel(context.Background())
	r, err := PrepareMQ(setup, c, MQOptions{SetupTimeout: 30 * time.Millisecond, CloseTimeout: time.Second})
	cancel()
	if err != nil || r == nil || r.Publisher() == nil {
		t.Fatal("offline API preparation incorrectly required broker readiness", err)
	}
	t.Cleanup(func() {
		if err := r.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	for i := 0; i < 2; i++ {
		select {
		case <-accepted:
		case <-time.After(time.Second):
			t.Fatal("publisher recovery did not survive setup cancellation")
		}
	}
	wait, cancelWait := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancelWait()
	if _, err := r.Publisher().WaitReady(wait); !errors.Is(err, context.DeadlineExceeded) || r.Publisher().Status().Ready {
		t.Fatal("offline publisher claimed readiness", err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMQResourcesConcurrentCloseJoinsSingleAttempt(t *testing.T) {
	var handles []*fakeMQResource
	var life context.Context
	r, err := prepareMQ(context.Background(), mqTestConfig(config.ModeAll), DefaultMQOptions(), fakeMQFactories(&handles, &life))
	if err != nil {
		t.Fatal(err)
	}
	release, started := make(chan struct{}), make(chan struct{}, len(handles))
	for _, h := range handles {
		h.close = func(ctx context.Context) error {
			started <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	results := make(chan error, 8)
	for i := 0; i < cap(results); i++ {
		go func() { results <- r.Close(context.Background()) }()
	}
	for range handles {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("concurrent close did not reach each resource")
		}
	}
	close(release)
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	assertMQClosed(t, handles, 1)
}
