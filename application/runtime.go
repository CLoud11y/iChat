package application

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"iChat/config"
	"iChat/messaging/rabbitmq"
)

var (
	ErrRuntimeOptions  = errors.New("invalid runtime execution options")
	ErrRuntimeStart    = errors.New("runtime startup failed")
	ErrRuntimeStopped  = errors.New("runtime service stopped unexpectedly")
	ErrRuntimeShutdown = errors.New("runtime shutdown failed")
)

// StartupTimeout bounds MQ preparation, all factories and all activations
// together. Shutdown has one drain budget, followed by one close budget shared
// by services and MQ. These are assembly inputs, not new YAML/env parameters.
type RuntimeOptions struct {
	MQ             MQOptions
	StartupTimeout time.Duration
	DrainTimeout   time.Duration
	CloseTimeout   time.Duration
}

func DefaultRuntimeOptions() RuntimeOptions {
	return RuntimeOptions{MQ: DefaultMQOptions(), StartupTimeout: 30 * time.Second, DrainTimeout: 10 * time.Second, CloseTimeout: 5 * time.Second}
}

func validRuntimeOptions(o RuntimeOptions) bool {
	valid := func(d time.Duration) bool { return d > 0 && d <= time.Minute }
	return validMQOptions(o.MQ) && valid(o.StartupTimeout) && valid(o.DrainTimeout) && valid(o.CloseTimeout)
}

type runtimeEntry struct {
	role      RuntimeRole
	service   RuntimeService
	attempted bool
	done      <-chan struct{}
}

// RunRuntime owns modern services and their MQ dependency until cancellation
// or an unexpected service exit. Factories must prepare idle services and obey
// their context; lifecycle methods must satisfy RuntimeService's bounded-call
// contract. This executor does not implement business roles or install signals.
// Legacy remains in its existing bootstrap and is rejected by this entry point.
func RunRuntime(ctx context.Context, c config.Config, f RuntimeFactories, o RuntimeOptions) error {
	return runRuntime(ctx, c, f, o, PrepareMQ)
}

func runRuntime(ctx context.Context, c config.Config, f RuntimeFactories, o RuntimeOptions, prepare mqPreparer) (err error) {
	if ctx == nil || !validRuntimeOptions(o) || prepare == nil {
		return ErrRuntimeOptions
	}
	a, err := f.Check(c) // No dependency construction before every role exists.
	if err != nil {
		return err
	}
	if a.Mode() == config.ModeLegacy {
		return fmt.Errorf("%w: legacy uses its existing bootstrap", ErrRuntimeOptions)
	}
	plan, err := c.MQRuntime()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	setup, cancelSetup := context.WithTimeout(ctx, o.StartupTimeout)
	defer cancelSetup()
	// Process cancellation starts drain; it must not cancel in-flight work or
	// MQ first. The role lifetime is cancelled only AFTER the drain phase.
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	readiness := &runtimeHealth{ctx: ctx, mode: a.Mode()}
	var resources *MQResources
	var entries []*runtimeEntry
	defer func() {
		readiness.stop() // Withdraw health before any admission/drain callback.
		cancelSetup()
		err = errors.Join(err, shutdownRuntime(entries, resources, cancelLifetime, o))
	}()
	resources, err = prepare(setup, c, o.MQ)
	if err != nil || resources == nil {
		return &runtimeFailure{kind: ErrRuntimeStart, stage: "MQ preparation", cause: err}
	}
	inputs, err := runtimeRoleInputs(resources, plan)
	if err != nil {
		return &runtimeFailure{kind: ErrRuntimeStart, stage: "MQ role dependencies", cause: err}
	}
	services := make(map[RuntimeRole]RuntimeService, len(a.selected))
	for _, selected := range a.selected {
		if err := setup.Err(); err != nil {
			return &runtimeFailure{kind: ErrRuntimeStart, stage: "service preparation deadline", cause: err}
		}
		roleInputs := inputs[selected.role]
		roleInputs.Health = readiness
		service, buildErr := selected.factory(setup, roleInputs)
		if !nilRuntimeService(service) {
			for _, previous := range entries {
				if sameRuntimeService(previous.service, service) {
					// Keep just the first ownership record, including handle+error.
					return &runtimeFailure{kind: ErrRuntimeStart, stage: "shared service handle", cause: errors.Join(ErrRuntimeServices, buildErr)}
				}
			}
			entries = append(entries, &runtimeEntry{role: selected.role, service: service})
			services[selected.role] = service
		}
		if buildErr != nil || nilRuntimeService(service) {
			if buildErr == nil {
				buildErr = ErrRuntimeServices
			}
			return &runtimeFailure{kind: ErrRuntimeStart, stage: "construct " + string(selected.role), cause: buildErr}
		}
	}
	if err := a.ValidateServices(services); err != nil {
		return &runtimeFailure{kind: ErrRuntimeStart, stage: "service instances", cause: err}
	}
	// Audit/notification machinery and consumers start before the accepting API.
	for _, role := range []RuntimeRole{RoleOutbox, RoleQuarantine, RolePersistence, RoleAPI} {
		for _, entry := range entries {
			if entry.role != role {
				continue
			}
			if err := setup.Err(); err != nil {
				return &runtimeFailure{kind: ErrRuntimeStart, stage: "activation deadline", cause: err}
			}
			if err := exitedRuntimeService(entries); err != nil {
				return err
			}
			entry.attempted = true // A failed Start may already own admitted work.
			if err := entry.service.Start(setup, lifetime); err != nil {
				return &runtimeFailure{kind: ErrRuntimeStart, stage: "activate " + string(role), cause: err}
			}
			entry.done = entry.service.Done()
			if entry.done == nil {
				return &runtimeFailure{kind: ErrRuntimeStart, stage: "missing Done for " + string(role), cause: ErrRuntimeServices}
			}
		}
	}
	if err := setup.Err(); err != nil {
		return &runtimeFailure{kind: ErrRuntimeStart, stage: "activation handoff", cause: err}
	}
	if err := exitedRuntimeService(entries); err != nil {
		return err
	}
	readiness.activate(resources, entries)
	cancelSetup() // Successful services retain lifetime, never the setup deadline.
	return awaitRuntimeStop(ctx, entries)
}

func runtimeRoleInputs(resources *MQResources, plan config.MQRuntimePlan) (map[RuntimeRole]RoleDependencies, error) {
	if resources.Mode() != plan.Mode {
		return nil, ErrRuntimeServices
	}
	inputs := map[RuntimeRole]RoleDependencies{}
	publisher := resources.Publisher()
	if nilRoleCapability(publisher) != (plan.Publisher == nil) {
		return nil, ErrRuntimeServices
	}
	if !nilRoleCapability(publisher) {
		inputs[RoleAPI] = RoleDependencies{Publisher: publisher}
	}
	consumers := resources.Consumers()
	expected := 0
	for _, group := range plan.Consumers {
		expected += group.Settings.Workers
	}
	if len(consumers) != expected {
		return nil, ErrRuntimeServices
	}
	for _, group := range plan.Consumers {
		role := RolePersistence
		if group.Queue == rabbitmq.QuarantineQueue {
			role = RoleQuarantine
		}
		var matching []PreparedConsumer
		for _, consumer := range consumers {
			if consumer.Queue == group.Queue {
				if nilRoleCapability(consumer.Session) || consumer.Prefetch != group.Settings.Prefetch {
					return nil, ErrRuntimeServices
				}
				matching = append(matching, consumer)
			}
		}
		if len(matching) != group.Settings.Workers {
			return nil, ErrRuntimeServices
		}
		inputs[role] = RoleDependencies{Consumers: matching}
		if role == RolePersistence {
			input := inputs[role]
			input.PersistenceConsumers = resources.persistence
			inputs[role] = input
		}
		if role == RoleQuarantine && resources.quarantine != nil {
			input := inputs[role]
			input.QuarantineConsumers = &QuarantineConsumerSource{source: resources.quarantine}
			inputs[role] = input
		}
	}
	return inputs, nil
}

func nilRoleCapability(capability any) bool {
	if capability == nil {
		return true
	}
	v := reflect.ValueOf(capability)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func sameRuntimeService(a, b RuntimeService) bool {
	t := reflect.TypeOf(a)
	return t == reflect.TypeOf(b) && t.Comparable() && a == b
}

func exitedRuntimeService(entries []*runtimeEntry) error {
	for _, entry := range entries {
		if entry.done == nil {
			continue
		}
		select {
		case _, open := <-entry.done:
			return runtimeServiceExit(entry, open)
		default:
		}
	}
	return nil
}

func runtimeServiceExit(entry *runtimeEntry, open bool) error {
	if open {
		return &runtimeFailure{kind: ErrRuntimeStopped, stage: "Done sent a value for " + string(entry.role), cause: ErrRuntimeServices}
	}
	return &runtimeFailure{kind: ErrRuntimeStopped, stage: string(entry.role), cause: entry.service.Err()}
}

func awaitRuntimeStop(ctx context.Context, entries []*runtimeEntry) error {
	if ctx.Err() != nil {
		return nil
	}
	cases := []reflect.SelectCase{{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())}}
	for _, entry := range entries {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(entry.done)})
	}
	index, _, open := reflect.Select(cases)
	if index == 0 || ctx.Err() != nil {
		return nil
	}
	return runtimeServiceExit(entries[index-1], open)
}

func shutdownRuntime(entries []*runtimeEntry, resources *MQResources, cancel context.CancelFunc, o RuntimeOptions) error {
	var attempted []*runtimeEntry
	// Stop every admission path before any service can begin its drain.
	for _, role := range []RuntimeRole{RoleAPI, RolePersistence, RoleQuarantine, RoleOutbox} {
		for _, entry := range entries {
			if entry.role == role && entry.attempted {
				entry.service.StopAdmission()
				attempted = append(attempted, entry)
			}
		}
	}
	var results []error
	if len(attempted) != 0 {
		drain, cancelDrain := context.WithTimeout(context.Background(), o.DrainTimeout)
		results = append(results, callRuntimeServices(drain, attempted, "drain", func(ctx context.Context, s RuntimeService) error { return s.Drain(ctx) }))
		cancelDrain()
	}
	cancel() // Commit/ACK opportunity ends only after all drain calls return.
	closeCtx, cancelClose := context.WithTimeout(context.Background(), o.CloseTimeout)
	defer cancelClose()
	results = append(results, callRuntimeServices(closeCtx, entries, "close", func(ctx context.Context, s RuntimeService) error { return s.Close(ctx) }))
	if resources != nil {
		// Still reach every MQ Close when service cleanup consumed the budget.
		if err := resources.Close(closeCtx); err != nil {
			results = append(results, &runtimeFailure{kind: ErrRuntimeShutdown, stage: "close MQ", cause: err})
		}
	}
	return errors.Join(results...)
}

func callRuntimeServices(ctx context.Context, entries []*runtimeEntry, stage string, call func(context.Context, RuntimeService) error) error {
	results := make([]error, len(entries))
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := call(ctx, entry.service); err != nil {
				results[i] = &runtimeFailure{kind: ErrRuntimeShutdown, stage: stage + " " + string(entry.role), cause: err}
			}
		}()
	}
	wg.Wait() // Private adapters must obey the context; no abandoned call goroutines.
	return errors.Join(results...)
}

type runtimeFailure struct {
	kind  error
	stage string
	cause error
}

func (e *runtimeFailure) Error() string    { return fmt.Sprintf("%s: %s", e.kind, e.stage) }
func (e *runtimeFailure) String() string   { return e.Error() }
func (e *runtimeFailure) GoString() string { return e.Error() }
func (e *runtimeFailure) Unwrap() []error  { return []error{e.kind, e.cause} }
