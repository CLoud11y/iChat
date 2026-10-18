package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/config"
	"iChat/health"
	"iChat/messaging/rabbitmq"
)

type observedRuntimeService struct {
	*scriptedRuntimeService
	accept, auth, consumer atomic.Bool
}

func (s *observedRuntimeService) AcceptanceReady() bool     { return s.accept.Load() }
func (s *observedRuntimeService) AuthenticationReady() bool { return s.auth.Load() }
func (s *observedRuntimeService) ConsumerReady() bool       { return s.consumer.Load() }

type observedPublisher struct {
	*fakeMQPublisher
	mu     sync.Mutex
	status rabbitmq.SupervisorStatus
}

func (p *observedPublisher) Status() rabbitmq.SupervisorStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}
func (p *observedPublisher) set(status rabbitmq.SupervisorStatus) {
	p.mu.Lock()
	p.status = status
	p.mu.Unlock()
}

type readinessHarness struct {
	*runtimeHarness
	services  map[RuntimeRole]*observedRuntimeService
	publisher *observedPublisher
	missing   RuntimeRole
}

func newReadinessHarness() *readinessHarness {
	h := &readinessHarness{runtimeHarness: newRuntimeHarness(), services: map[RuntimeRole]*observedRuntimeService{}}
	for role, original := range h.runtimeHarness.services {
		s := &observedRuntimeService{scriptedRuntimeService: original}
		s.accept.Store(true)
		s.auth.Store(true)
		s.consumer.Store(true)
		h.services[role] = s
	}
	return h
}

func (h *readinessHarness) factories(t *testing.T) RuntimeFactories {
	base := h.runtimeHarness.factories()
	wrap := func(role RuntimeRole, f RuntimeFactory) RuntimeFactory {
		return func(ctx context.Context, inputs RoleDependencies) (RuntimeService, error) {
			if inputs.Health == nil {
				t.Error("factory did not receive health observation")
			}
			status := inputs.Health.RuntimeReadiness()
			if status.Acceptance == health.Ready || status.Consumers == health.Ready {
				t.Error("construction incorrectly established readiness")
			}
			s, err := f(ctx, inputs)
			if h.missing == role {
				return s, err // Start alone does not establish business readiness.
			}
			return h.services[role], err
		}
	}
	return RuntimeFactories{API: wrap(RoleAPI, base.API), Persistence: wrap(RolePersistence, base.Persistence), Quarantine: wrap(RoleQuarantine, base.Quarantine), Outbox: wrap(RoleOutbox, base.Outbox)}
}

func (h *readinessHarness) prepare(ctx context.Context, c config.Config, o MQOptions) (*MQResources, error) {
	r, err := h.runtimeHarness.prepare(ctx, c, o)
	if r != nil && r.publisher != nil {
		h.publisher = &observedPublisher{fakeMQPublisher: r.publisher.(*fakeMQPublisher), status: rabbitmq.SupervisorStatus{State: rabbitmq.SupervisorReady, Ready: true, ConnectionEpoch: 1}}
		r.publisher = h.publisher
	}
	return r, err
}

func startReadinessHarness(t *testing.T, h *readinessHarness, mode config.RuntimeMode) (context.CancelFunc, <-chan error, health.RuntimeHealth) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- runRuntime(ctx, mqTestConfig(mode), h.factories(t), DefaultRuntimeOptions(), h.prepare)
	}()
	t.Cleanup(cancel)
	var last *scriptedRuntimeService
	for range runtimeExpectedRoles(mode) {
		select {
		case last = <-h.started:
		case err := <-result:
			t.Fatal("readiness runtime failed during start", err)
		case <-time.After(2 * time.Second):
			t.Fatal("readiness activation did not finish")
		}
	}
	select {
	case <-last.setup.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("readiness handoff did not finish")
	}
	return cancel, result, h.inputs[last.role].Health
}

func TestRuntimeHealthModesAndActivation(t *testing.T) {
	for _, mode := range []config.RuntimeMode{config.ModeAPI, config.ModeWorker, config.ModeAll} {
		t.Run(string(mode), func(t *testing.T) {
			h := newReadinessHarness()
			for role, service := range h.services {
				service.start = func(context.Context, context.Context) error {
					status := h.inputs[role].Health.RuntimeReadiness()
					if status.Acceptance == health.Ready || status.Consumers == health.Ready {
						return errors.New("partially activated runtime reported ready")
					}
					return nil
				}
			}
			cancel, result, observed := startReadinessHarness(t, h, mode)
			want := health.RuntimeStatus{Acceptance: health.Ready, Consumers: health.Ready}
			if mode == config.ModeWorker {
				want.Acceptance = health.NotConfigured
			}
			if mode == config.ModeAPI {
				want.Consumers = health.NotConfigured
			}
			if status := observed.RuntimeReadiness(); status != want {
				t.Fatal("incorrect role readiness", status, want)
			}
			for _, inputs := range h.inputs {
				if inputs.Health != observed {
					t.Fatal("roles received different health owners")
				}
			}
			cancel()
			if err := runtimeResult(t, result); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeHealthAcceptanceRequiresBusinessAuthAndCurrentPublisher(t *testing.T) {
	h := newReadinessHarness()
	cancel, result, observed := startReadinessHarness(t, h, config.ModeAPI)
	defer func() { cancel(); runtimeResult(t, result) }()
	api := h.services[RoleAPI]
	api.accept.Store(false)
	if got := observed.RuntimeReadiness().Acceptance; got != health.ServiceUnavailable {
		t.Fatal("missing accepted handler was ready", got)
	}
	api.accept.Store(true)
	api.auth.Store(false)
	if got := observed.RuntimeReadiness().Acceptance; got != health.AuthenticationUnavailable {
		t.Fatal("unavailable authentication was ready", got)
	}
	api.auth.Store(true)
	for _, status := range []rabbitmq.SupervisorStatus{
		{},
		{Ready: true, State: rabbitmq.SupervisorBackoff, ConnectionEpoch: 1},
		{Ready: true, State: rabbitmq.SupervisorReady},
		{State: rabbitmq.SupervisorReady, ConnectionEpoch: 1},
	} {
		h.publisher.set(status)
		if got := observed.RuntimeReadiness().Acceptance; got != health.PublisherUnavailable {
			t.Fatal("invalid publisher snapshot was ready", status, got)
		}
	}
	h.publisher.set(rabbitmq.SupervisorStatus{State: rabbitmq.SupervisorReady, Ready: true, ConnectionEpoch: 4})
	if got := observed.RuntimeReadiness().Acceptance; got != health.Ready {
		t.Fatal("recovered auth/publisher did not restore observation", got)
	}
}

func TestRuntimeHealthConsumersAreSeparateFromAcceptance(t *testing.T) {
	h := newReadinessHarness()
	cancel, result, observed := startReadinessHarness(t, h, config.ModeAll)
	defer func() { cancel(); runtimeResult(t, result) }()
	for _, role := range []RuntimeRole{RolePersistence, RoleQuarantine} {
		h.services[role].consumer.Store(false) // Includes local storage outage.
		if status := observed.RuntimeReadiness(); status.Consumers != health.ConsumerUnavailable || status.Acceptance != health.Ready {
			t.Fatal("local consumer/storage outage incorrectly gated acceptance", status)
		}
		h.services[role].consumer.Store(true)
	}
	consumer := h.mq.consumers[0].Session.(*fakeMQConsumer)
	consumer.once.Do(func() { close(consumer.done) })
	if status := observed.RuntimeReadiness(); status.Consumers != health.ConsumerUnavailable || status.Acceptance != health.Ready {
		t.Fatal("closed consumer connection was confused with publisher readiness", status)
	}
	h.publisher.set(rabbitmq.SupervisorStatus{})
	if status := observed.RuntimeReadiness(); status.Consumers != health.ConsumerUnavailable || status.Acceptance != health.PublisherUnavailable {
		t.Fatal("separate transport failures were not observed", status)
	}
}

func TestRuntimeHealthMissingBusinessCapabilitiesFailClosed(t *testing.T) {
	for _, role := range []RuntimeRole{RoleAPI, RolePersistence, RoleQuarantine} {
		t.Run(string(role), func(t *testing.T) {
			h := newReadinessHarness()
			h.missing = role
			cancel, result, observed := startReadinessHarness(t, h, config.ModeAll)
			status := observed.RuntimeReadiness()
			if role == RoleAPI && status.Acceptance != health.ServiceUnavailable {
				t.Fatal("Start was mistaken for acceptance capability", status)
			}
			if role != RoleAPI && (status.Consumers != health.ConsumerUnavailable || status.Acceptance != health.Ready) {
				t.Fatal("idle consumer service was mistaken for business readiness", status)
			}
			cancel()
			if err := runtimeResult(t, result); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeHealthWithdrawsBeforeDrainAndStaysStopped(t *testing.T) {
	h := newReadinessHarness()
	for _, service := range h.services {
		service.stop = func() {
			status := h.inputs[RoleAPI].Health.RuntimeReadiness()
			if status.Acceptance != health.Stopping || status.Consumers != health.Stopping {
				t.Error("admission callback saw ready during shutdown", status)
			}
		}
		service.drain = func(context.Context) error {
			if status := h.inputs[RoleAPI].Health.RuntimeReadiness(); status.Acceptance != health.Stopping || status.Consumers != health.Stopping {
				return errors.New("drain still reported ready")
			}
			if h.mqLife.Err() != nil {
				return errors.New("withdrawing readiness canceled MQ before drain")
			}
			return nil
		}
	}
	cancel, result, observed := startReadinessHarness(t, h, config.ModeAll)
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 100 {
				observed.RuntimeReadiness()
			}
		}()
	}
	cancel()
	if got := observed.RuntimeReadiness().Acceptance; got != health.Stopping {
		t.Fatal("cancelled process remained ready before callbacks", got)
	}
	readers.Wait()
	if err := runtimeResult(t, result); err != nil {
		t.Fatal(err)
	}
	if status := observed.RuntimeReadiness(); status.Acceptance != health.Stopping || status.Consumers != health.Stopping {
		t.Fatal("late health observation revived shutdown runtime", status)
	}
}

func TestRuntimeHealthStartupAndUnexpectedExitNeverStayReady(t *testing.T) {
	t.Run("startup_failure", func(t *testing.T) {
		h := newReadinessHarness()
		failure := errors.New("PRIVATE_STARTUP_FAILURE")
		h.services[RoleAPI].start = func(context.Context, context.Context) error { return failure }
		err := runRuntime(context.Background(), mqTestConfig(config.ModeAPI), h.factories(t), DefaultRuntimeOptions(), h.prepare)
		if !errors.Is(err, failure) || h.inputs[RoleAPI].Health.RuntimeReadiness().Acceptance != health.Stopping {
			t.Fatal("startup failure left a healthy observation", err)
		}
	})
	t.Run("service_exit", func(t *testing.T) {
		h := newReadinessHarness()
		cancel, result, observed := startReadinessHarness(t, h, config.ModeAll)
		defer cancel()
		api := h.services[RoleAPI]
		api.once.Do(func() { close(api.done) })
		if got := observed.RuntimeReadiness().Acceptance; got == health.Ready {
			t.Fatal("exited service stayed ready")
		}
		if err := runtimeResult(t, result); !errors.Is(err, ErrRuntimeStopped) {
			t.Fatal("unexpected exit was hidden", err)
		}
	})
}

func TestRuntimeHealthMissingTransportAndClosedResourcesFailClosed(t *testing.T) {
	h := newReadinessHarness()
	cancel, result, observed := startReadinessHarness(t, h, config.ModeAll)
	defer func() { cancel(); runtimeResult(t, result) }()
	var typedNil *observedPublisher
	h.mq.mu.Lock()
	h.mq.publisher = typedNil
	h.mq.mu.Unlock()
	if got := observed.RuntimeReadiness().Acceptance; got != health.PublisherUnavailable {
		t.Fatal("typed nil publisher was ready", got)
	}
	h.mq.mu.Lock()
	h.mq.publisher = h.publisher
	var nilConsumer *fakeMQConsumer
	h.mq.consumers[0].Session = nilConsumer
	h.mq.mu.Unlock()
	if status := observed.RuntimeReadiness(); status.Acceptance != health.Ready || status.Consumers != health.ConsumerUnavailable {
		t.Fatal("typed nil consumer became ready or affected acceptance", status)
	}
	if err := h.mq.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := observed.RuntimeReadiness(); status.Acceptance != health.Stopping || status.Consumers != health.Stopping {
		t.Fatal("closed MQ group retained readiness", status)
	}
}

func TestRuntimeHealthFormattingDoesNotExposeContext(t *testing.T) {
	type healthContextKey struct{}
	h := &runtimeHealth{ctx: context.WithValue(context.Background(), healthContextKey{}, "PRIVATE_CONTEXT_VALUE"), mode: config.ModeAPI}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if text := fmt.Sprintf(format, h); strings.Contains(text, "PRIVATE") || text != "runtime health observer" {
			t.Fatal("health capability exposed its context", text)
		}
	}
}
