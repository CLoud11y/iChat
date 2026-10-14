package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"iChat/config"
)

func testMQLifecycle(t *testing.T, ctx context.Context, mode config.RuntimeMode, o MQOptions) (*MQLifecycle, []*fakeMQResource, context.Context) {
	t.Helper()
	var handles []*fakeMQResource
	var publisherLifetime context.Context
	f := fakeMQFactories(&handles, &publisherLifetime)
	m, err := startMQLifecycle(ctx, mqTestConfig(mode), o, func(ctx context.Context, c config.Config, o MQOptions) (*MQResources, error) {
		return prepareMQ(ctx, c, o, f)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m, handles, publisherLifetime
}

func TestMQLifecycleCancellationClosesSelectedResources(t *testing.T) {
	for _, mode := range []config.RuntimeMode{config.ModeLegacy, config.ModeAPI, config.ModeWorker, config.ModeAll} {
		t.Run(string(mode), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m, handles, publisherLifetime := testMQLifecycle(t, ctx, mode, DefaultMQOptions())
			if m.Mode() != mode {
				t.Fatal("wrong lifecycle mode")
			}
			if mode == config.ModeAPI || mode == config.ModeAll {
				if m.Publisher() == nil || publisherLifetime.Err() != nil {
					t.Fatal("publisher lifetime not transferred")
				}
			}
			if mode == config.ModeWorker || mode == config.ModeAll {
				copy := m.Consumers()
				if len(copy) != 4 {
					t.Fatal("wrong consumer count")
				}
				copy[0] = PreparedConsumer{}
				if m.Consumers()[0].Session == nil {
					t.Fatal("caller mutated managed consumer ownership")
				}
			}
			cancel()
			if m.Publisher() != nil || len(m.Consumers()) != 0 {
				t.Fatal("cancelled lifecycle still exposes new capabilities")
			}
			wait, cancelWait := context.WithTimeout(context.Background(), time.Second)
			defer cancelWait()
			if err := m.Wait(wait); err != nil {
				t.Fatal(err)
			}
			select {
			case <-m.Done():
			default:
				t.Fatal("lifecycle owner not joined")
			}
			assertMQClosed(t, handles, 1)
			if publisherLifetime != nil && publisherLifetime.Err() == nil {
				t.Fatal("publisher recovery outlived lifecycle shutdown")
			}
			if err := m.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertMQClosed(t, handles, 1)
		})
	}
}

func TestMQLifecycleInvalidInputsBeforePreparation(t *testing.T) {
	for _, name := range []string{"nil_context", "cancelled", "bad_options", "missing_prepare"} {
		t.Run(name, func(t *testing.T) {
			ctx, o := context.Background(), DefaultMQOptions()
			calls := 0
			prepare := mqPreparer(func(context.Context, config.Config, MQOptions) (*MQResources, error) {
				calls++
				return nil, nil
			})
			switch name {
			case "nil_context":
				ctx = nil
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "bad_options":
				o.CloseTimeout = 0
			case "missing_prepare":
				prepare = nil
			}
			if m, err := startMQLifecycle(ctx, mqTestConfig(config.ModeAll), o, prepare); m != nil || err == nil || calls != 0 {
				t.Fatal("invalid input reached preparation", err)
			}
		})
	}
	// Valid lifetime still cannot bypass role/config validation in PrepareMQ.
	c := mqTestConfig(config.ModeAll)
	c.RABBITMQ.ConsumerWorkers = 0
	if m, err := StartMQLifecycle(context.Background(), c, DefaultMQOptions()); m != nil || !errors.Is(err, config.ErrRabbitMQConfig) {
		t.Fatal("invalid role configuration reached real connection setup", err)
	}
}

func TestMQLifecycleFailedOrCancelledHandoffRollsBack(t *testing.T) {
	secret := errors.New("amqp://user:secret-factory@host; private payload")
	for _, name := range []string{"prepare_error", "handle_and_error", "handle_and_cleanup_error", "cancelled_handoff", "nil_handle"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var handles []*fakeMQResource
			var life context.Context
			f := fakeMQFactories(&handles, &life)
			prepare := func(ctx context.Context, c config.Config, o MQOptions) (*MQResources, error) {
				if name == "prepare_error" {
					return nil, secret
				}
				if name == "nil_handle" {
					return nil, nil
				}
				r, err := prepareMQ(ctx, c, o, f)
				if err != nil {
					t.Fatal(err)
				}
				if name == "cancelled_handoff" {
					cancel()
					return r, nil
				}
				if name == "handle_and_cleanup_error" {
					for _, h := range handles {
						h.close = func(context.Context) error { return secret }
					}
				}
				return r, secret
			}
			m, err := startMQLifecycle(ctx, mqTestConfig(config.ModeAll), DefaultMQOptions(), prepare)
			if m != nil || !errors.Is(err, ErrMQPrepare) {
				t.Fatal("failed handoff returned an owner", err)
			}
			if name == "cancelled_handoff" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancelled handoff lost cancellation", err)
			}
			if name == "handle_and_cleanup_error" && (!errors.Is(err, ErrMQClose) || !errors.Is(err, secret)) {
				t.Fatal("handoff cleanup error was swallowed", err)
			}
			for _, format := range []string{"%v", "%+v", "%#v"} {
				if strings.Contains(fmt.Sprintf(format, err), "secret-factory") {
					t.Fatal("handoff error leaked credentials")
				}
			}
			assertMQClosed(t, handles, 1)
		})
	}
}

func TestMQLifecycleWaitCancellationDoesNotStopResources(t *testing.T) {
	m, handles, life := testMQLifecycle(t, context.Background(), config.ModeAll, DefaultMQOptions())
	if m.Wait(nil) != ErrMQOptions || m.Close(nil) != ErrMQOptions {
		t.Fatal("nil observer context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("wait ignored cancellation", err)
	}
	if m.Publisher() == nil || len(m.Consumers()) != 4 || life.Err() != nil {
		t.Fatal("observer cancellation stopped resource lifetime")
	}
	assertMQClosed(t, handles, 0)
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertMQClosed(t, handles, 1)
}

func TestMQLifecycleSetupDeadlineDoesNotEndTransferredLifetime(t *testing.T) {
	m, handles, life := testMQLifecycle(t, context.Background(), config.ModeAll, MQOptions{SetupTimeout: 20 * time.Millisecond, CloseTimeout: time.Second})
	wait, cancelWait := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelWait()
	if err := m.Wait(wait); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("setup deadline unexpectedly ended the lifetime", err)
	}
	if life.Err() != nil || m.Publisher() == nil || len(m.Consumers()) != 4 {
		t.Fatal("completed setup context still owned resource lifetime")
	}
	assertMQClosed(t, handles, 0)
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertMQClosed(t, handles, 1)
}

func TestMQLifecycleShortCloseKeepsOwnedCleanupAndCanJoinLater(t *testing.T) {
	m, handles, _ := testMQLifecycle(t, context.Background(), config.ModeAll, DefaultMQOptions())
	release, started := make(chan struct{}), make(chan struct{}, len(handles))
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	for _, h := range handles {
		h.close = func(ctx context.Context) error {
			if ctx.Err() != nil {
				return errors.New("cleanup inherited expired observer")
			}
			started <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Close(ctx); !errors.Is(err, ErrMQClose) || !errors.Is(err, context.Canceled) {
		t.Fatal("short observer did not return its cancellation", err)
	}
	for range handles {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("early-returning Close abandoned cleanup")
		}
	}
	select {
	case <-m.Done():
		t.Fatal("Done closed before cleanup was joined")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := m.Close(context.Background()); err != nil {
		t.Fatal("later observer could not join cleanup", err)
	}
	assertMQClosed(t, handles, 1)
}

func TestMQLifecycleDeadlineProvidesIndependentCleanupBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var handles []*fakeMQResource
	var life context.Context
	f := fakeMQFactories(&handles, &life)
	m, err := startMQLifecycle(ctx, mqTestConfig(config.ModeAll), MQOptions{SetupTimeout: time.Second, CloseTimeout: time.Second}, func(ctx context.Context, c config.Config, o MQOptions) (*MQResources, error) {
		r, err := prepareMQ(ctx, c, o, f)
		// Configure before the lifecycle goroutine can observe the handles.
		for _, h := range handles {
			h.close = func(ctx context.Context) error {
				if ctx.Err() != nil {
					return errors.New("resource deadline inherited expired lifetime")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 500*time.Millisecond {
					return errors.New("lifetime consumed resource cleanup budget")
				}
				return nil
			}
		}
		return r, err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	if err := m.Wait(context.Background()); err != nil || ctx.Err() != context.DeadlineExceeded {
		t.Fatal("lifetime deadline did not trigger independent cleanup", err)
	}
	assertMQClosed(t, handles, 1)
}

func TestMQLifecycleCloseFailureIsVisibleAndRetryable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, handles, _ := testMQLifecycle(t, ctx, config.ModeAll, MQOptions{SetupTimeout: time.Second, CloseTimeout: 40 * time.Millisecond})
	joined := make(chan struct{}, len(handles))
	for _, h := range handles {
		h.close = func(ctx context.Context) error {
			<-ctx.Done()
			joined <- struct{}{}
			return ctx.Err()
		}
	}
	cancel()
	wait, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if err := m.Wait(wait); !errors.Is(err, ErrMQClose) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cleanup timeout was swallowed", err)
	}
	if len(joined) != len(handles) {
		t.Fatal("lifecycle did not join all child close calls")
	}
	assertMQClosed(t, handles, 1)
	for _, h := range handles {
		h.close = nil
	}
	if err := m.Close(context.Background()); !errors.Is(err, ErrMQClose) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("retry erased the first observed cleanup failure", err)
	}
	assertMQClosed(t, handles, 2)
	if err := m.Wait(context.Background()); !errors.Is(err, ErrMQClose) {
		t.Fatal("owner completion result changed after retry", err)
	}
}

func TestMQLifecycleConcurrentCloseJoinsOneOwner(t *testing.T) {
	m, handles, _ := testMQLifecycle(t, context.Background(), config.ModeAll, DefaultMQOptions())
	results := make(chan error, 8)
	for i := 0; i < cap(results); i++ {
		go func() { results <- m.Close(context.Background()) }()
	}
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	assertMQClosed(t, handles, 1)
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, m), "secret") && !strings.Contains(fmt.Sprintf(format, m), "redacted") {
			t.Fatal("managed resources leaked credentials")
		}
	}
}
