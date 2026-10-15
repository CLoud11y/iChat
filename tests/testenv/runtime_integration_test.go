//go:build integration

package testenv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/application"
	"iChat/config"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"

	amqp "github.com/rabbitmq/amqp091-go"
)

// These are fixture roles, with no business handler or subscription. Only the
// accepting fixture role publishes during drain to prove the MQ lifetime order.
type fixtureRuntimeService struct {
	inputs   application.RoleDependencies
	setup    context.Context
	lifetime context.Context
	done     chan struct{}
	once     sync.Once
	start    func(context.Context) error
	drain    func(context.Context) error
	starts   atomic.Int32
	stops    atomic.Int32
	closes   atomic.Int32
}

func (s *fixtureRuntimeService) Start(setup, lifetime context.Context) error {
	s.setup, s.lifetime = setup, lifetime
	s.starts.Add(1)
	if s.start != nil {
		return s.start(setup)
	}
	return nil
}
func (s *fixtureRuntimeService) StopAdmission() { s.stops.Add(1) }
func (s *fixtureRuntimeService) Drain(ctx context.Context) error {
	if s.lifetime.Err() != nil || ctx.Err() != nil {
		return errors.New("drain inherited stopped role/process lifetime")
	}
	if s.drain != nil {
		return s.drain(ctx)
	}
	return nil
}
func (s *fixtureRuntimeService) Close(context.Context) error {
	s.closes.Add(1)
	s.once.Do(func() { close(s.done) })
	if s.lifetime != nil && s.lifetime.Err() == nil {
		return errors.New("service lifetime remained active during Close")
	}
	return nil
}
func (s *fixtureRuntimeService) Done() <-chan struct{} { return s.done }
func (*fixtureRuntimeService) Err() error              { return nil }

func TestRuntimeExecutionIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, broker := startRabbitFixture(t, ctx)
	m, err := rabbitmq.NewManagement("http://"+broker.ManagementAddress, rabbitAdminUser, env.password)
	if err != nil {
		t.Fatal(err)
	}
	control := openPublisherConnection(t, ctx, broker)
	ch, err := control.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := rabbitmq.EnsureLocal(ctx, ch, m, broker.VHost); err != nil {
		t.Fatal(err)
	}
	if err := ch.Confirm(false); err != nil {
		t.Fatal(err)
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	mainBody, auditBody := "pending-main:"+env.RunID(), "pending-audit:"+env.RunID()
	for _, item := range []struct{ exchange, key, body string }{
		{rabbitmq.CommandsExchange, rabbitmq.SubmitRoutingKey, mainBody},
		{rabbitmq.QuarantineExchange, rabbitmq.QuarantineRoutingKey, auditBody},
	} {
		if err := ch.PublishWithContext(ctx, item.exchange, item.key, true, false, amqp.Publishing{DeliveryMode: amqp.Persistent, Body: []byte(item.body)}); err != nil {
			t.Fatal(err)
		}
		select {
		case confirmation, ok := <-confirms:
			if !ok || !confirmation.Ack {
				t.Fatal("fixture pending record was not confirmed")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	c := config.Config{RUNTIME: config.RuntimeConfig{Mode: config.ModeAll}, RABBITMQ: config.RabbitMQConfig{
		URL: broker.AMQPURL(), VHost: broker.VHost,
		ManagementURL: "http://" + broker.ManagementAddress, ManagementUsername: rabbitAdminUser, ManagementPassword: env.password,
		PublisherSlots: 1, ConsumerWorkers: 2, ConsumerPrefetch: 3,
		PublishTimeout: 2 * time.Second, ConsumerOperationTimeout: 2 * time.Second,
		ConnectTimeout: 10 * time.Second, WriteTimeout: time.Second, CloseTimeout: time.Second, Heartbeat: time.Second,
		RetryMin: 50 * time.Millisecond, RetryMax: 200 * time.Millisecond,
	}}
	command, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: "runtime-drain:" + env.RunID() + ":中🙂\x00"})
	if err != nil {
		t.Fatal(err)
	}
	waitConnections := func(t *testing.T, want int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var records []struct{ User, VHost string }
			if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/connections", nil, &records); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, record := range records {
				if record.User == broker.Username && record.VHost == broker.VHost {
					count++
				}
			}
			if count == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("unexpected runtime MQ connection count", count, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	assertQueues := func(t *testing.T, mainCount int) {
		t.Helper()
		var subscriptions []any
		if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/consumers/"+url.PathEscape(broker.VHost), nil, &subscriptions); err != nil || len(subscriptions) != 0 {
			t.Fatal("fixture role created an unexpected subscription", err)
		}
		for _, item := range []struct {
			queue string
			ready int
		}{{rabbitmq.PersistQueue, mainCount}, {rabbitmq.QuarantineQueue, 1}} {
			deadline := time.Now().Add(15 * time.Second)
			for {
				var stats struct {
					Ready     *int `json:"messages_ready"`
					Unacked   *int `json:"messages_unacknowledged"`
					Consumers *int `json:"consumers"`
				}
				if _, err := env.rabbitAPI(ctx, http.MethodGet, "/api/queues/"+url.PathEscape(broker.VHost)+"/"+item.queue, nil, &stats); err != nil {
					t.Fatal(err)
				}
				if stats.Ready != nil && stats.Unacked != nil && stats.Consumers != nil && *stats.Ready == item.ready && *stats.Unacked == 0 && *stats.Consumers == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("runtime changed pending-message responsibility", item.queue)
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
	var accepted rabbitmq.PublishResult
	for _, scenario := range []string{"publish_during_drain", "construction_failure", "activation_failure", "drain_timeout"} {
		t.Run(scenario, func(t *testing.T) {
			services := map[application.RuntimeRole]*fixtureRuntimeService{}
			started := make(chan *fixtureRuntimeService, 4)
			failure := errors.New("controlled fixture role failure")
			factory := func(role application.RuntimeRole) application.RuntimeFactory {
				return func(setup context.Context, inputs application.RoleDependencies) (application.RuntimeService, error) {
					s := &fixtureRuntimeService{inputs: inputs, done: make(chan struct{})}
					services[role] = s
					s.start = func(ctx context.Context) error {
						if scenario == "activation_failure" && role == application.RolePersistence {
							return failure
						}
						if inputs.Publisher != nil {
							if _, err := inputs.Publisher.WaitReady(ctx); err != nil {
								return err
							}
						}
						for _, consumer := range inputs.Consumers {
							if err := consumer.Session.Verify(ctx); err != nil {
								return err
							}
						}
						started <- s
						return nil
					}
					s.drain = func(ctx context.Context) error {
						for _, other := range services {
							if other.starts.Load() != 0 && other.stops.Load() != 1 {
								return errors.New("role drained before all admission stopped")
							}
						}
						for _, consumer := range inputs.Consumers {
							if err := consumer.Session.Verify(ctx); err != nil {
								return err
							}
						}
						if role == application.RoleAPI && scenario == "drain_timeout" {
							<-ctx.Done()
							return ctx.Err()
						}
						if role == application.RoleAPI && scenario == "publish_during_drain" {
							var err error
							accepted, err = inputs.Publisher.Publish(ctx, command, time.Now())
							if err != nil || accepted.Outcome != rabbitmq.PublishAccepted {
								return fmt.Errorf("fixture drain publish did not receive confirm: %w", err)
							}
						}
						return nil
					}
					if scenario == "construction_failure" && role == application.RoleOutbox {
						return s, failure
					}
					return s, nil
				}
			}
			factories := application.RuntimeFactories{API: factory(application.RoleAPI), Persistence: factory(application.RolePersistence), Quarantine: factory(application.RoleQuarantine), Outbox: factory(application.RoleOutbox)}
			o := application.DefaultRuntimeOptions()
			o.StartupTimeout, o.DrainTimeout, o.CloseTimeout = 15*time.Second, 2*time.Second, 3*time.Second
			o.MQ.SetupTimeout, o.MQ.CloseTimeout = 10*time.Second, 2*time.Second
			if scenario == "drain_timeout" {
				o.DrainTimeout = 350 * time.Millisecond
			}
			run, stop := context.WithCancel(ctx)
			result := make(chan error, 1)
			go func() { result <- application.RunRuntime(run, c, factories, o) }()
			finished := false
			t.Cleanup(func() {
				stop()
				if !finished {
					select {
					case <-result:
					case <-time.After(20 * time.Second):
						t.Error("runtime cleanup did not finish")
					}
				}
			})
			if scenario == "publish_during_drain" || scenario == "drain_timeout" {
				var last *fixtureRuntimeService
				for i := 0; i < 4; i++ {
					select {
					case last = <-started:
					case err := <-result:
						finished = true
						t.Fatal("runtime did not activate", err)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				<-last.setup.Done() // Successful startup handoff, not process cancellation.
				waitConnections(t, 5)
				pending := 2
				if scenario == "publish_during_drain" {
					pending = 1
				}
				assertQueues(t, pending)
				stop()
			}
			var runErr error
			select {
			case runErr = <-result:
				finished = true
			case <-time.After(20 * time.Second):
				t.Fatal("runtime did not return after rollback/drain")
			}
			if scenario == "publish_during_drain" {
				if runErr != nil || accepted.Outcome != rabbitmq.PublishAccepted {
					t.Fatal("MQ was not usable through drain", runErr)
				}
			} else if scenario == "drain_timeout" {
				if !errors.Is(runErr, application.ErrRuntimeShutdown) || !errors.Is(runErr, context.DeadlineExceeded) {
					t.Fatal("drain timeout was lost", runErr)
				}
			} else if !errors.Is(runErr, application.ErrRuntimeStart) || !errors.Is(runErr, failure) || errors.Is(runErr, application.ErrRuntimeShutdown) {
				t.Fatal("failed startup did not cleanly roll back", runErr)
			}
			for _, service := range services {
				if service.closes.Load() != 1 || service.stops.Load() != service.starts.Load() {
					t.Fatal("partial/idle fixture service lost cleanup responsibility")
				}
			}
			waitConnections(t, 1)
			assertQueues(t, 2)
		})
	}
	seenPending, seenCommand := false, false
	for i := 0; i < 2; i++ {
		d := getTopologyDelivery(t, ctx, ch, rabbitmq.PersistQueue)
		if string(d.Body) == mainBody {
			seenPending = true
		} else {
			e, err := messaging.DecodeEnvelope(d.Body)
			if err != nil || e.CommandID != command.CommandID || e.PublishAttemptID != accepted.PublishAttemptID || e.PayloadHash != command.PayloadHash || e.Content != command.Content {
				t.Fatal("drain-confirmed command was lost or changed", err)
			}
			seenCommand = true
		}
		if d.DeliveryMode != amqp.Persistent {
			t.Fatal("pending/accepted record lost persistence")
		}
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		}
	}
	if !seenPending || !seenCommand {
		t.Fatal("runtime lost/duplicated a pending or confirmed record")
	}
	audit := getTopologyDelivery(t, ctx, ch, rabbitmq.QuarantineQueue)
	if string(audit.Body) != auditBody || audit.DeliveryMode != amqp.Persistent {
		t.Fatal("runtime changed pending audit responsibility")
	}
	if err := audit.Ack(false); err != nil {
		t.Fatal(err)
	}
}
