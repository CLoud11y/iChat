package application

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"iChat/config"
	"iChat/health"
)

var (
	ErrRuntimeNotAssembled = errors.New("requested runtime mode is not assembled yet")
	ErrRuntimeServices     = errors.New("invalid runtime service assembly")
)

type RuntimeRole string

const (
	RoleAPI         RuntimeRole = "api"
	RolePersistence RuntimeRole = "persistence_consumer"
	RoleQuarantine  RuntimeRole = "quarantine_consumer"
	RoleOutbox      RuntimeRole = "outbox"
)

// RuntimeService separates activation from admission stop, drain and forced
// close. Factories prepare idle services; none may start business work before
// the whole selected assembly passes ValidateServices.
type RuntimeService interface {
	// Start is bounded by setup; lifetime survives successful activation.
	Start(setup, lifetime context.Context) error
	// StopAdmission is synchronous and must not wait for work or perform RPCs.
	StopAdmission()
	// Drain waits for admitted work to finish, including commit and ACK.
	Drain(context.Context) error
	// Close is safe before Start and after a partially failed Start. It cancels
	// remaining work and joins owned tasks within its context.
	// It does not own the shared MQ group or shared storage dependency pools.
	Close(context.Context) error
	// Done reports service termination; Err is read after Done closes.
	Done() <-chan struct{}
	Err() error
}

// RoleDependencies supplies only MQ capabilities belonging to this role, plus
// a read-only runtime health observation. Other dependencies are captured by its
// explicit factory. The executor passes the command publisher only to API and
// matching queue groups only to consumers. Outbox has no MQ inputs.
type RoleDependencies struct {
	Publisher MQPublisher
	Consumers []PreparedConsumer
	// Health is a read-only aggregate. It is never proof of this role's own
	// readiness; RuntimeAcceptance/RuntimeConsumerReadiness must not consult it.
	Health health.RuntimeHealth
}

type RuntimeFactory func(context.Context, RoleDependencies) (RuntimeService, error)

type RuntimeFactories struct {
	API         RuntimeFactory
	Persistence RuntimeFactory
	Quarantine  RuntimeFactory
	Outbox      RuntimeFactory
}

type selectedRuntimeFactory struct {
	role    RuntimeRole
	factory RuntimeFactory
}

// RuntimeAssembly is a checked snapshot of selected factories, not active
// services, dependency readiness or proof that handlers implement the protocol.
// Its zero value cannot validate services. Fields are private so callers cannot
// forge roles or mutate the retained factory selection after Check.
type RuntimeAssembly struct {
	mode     config.RuntimeMode
	checked  bool
	selected []selectedRuntimeFactory
}

func (a RuntimeAssembly) Mode() config.RuntimeMode { return a.mode }

func (a RuntimeAssembly) Roles() []RuntimeRole {
	var roles []RuntimeRole
	for _, entry := range a.selected {
		roles = append(roles, entry.role)
	}
	return roles
}

// Check performs no I/O and invokes no factory or service method. Call it before
// constructing any external dependencies. Legacy uses its existing bootstrap,
// so its API/optional old worker do not require modern service factories.
func (f RuntimeFactories) Check(c config.Config) (RuntimeAssembly, error) {
	plan, err := c.RuntimePlan()
	if err != nil {
		return RuntimeAssembly{}, err
	}
	a := RuntimeAssembly{mode: plan.Mode, checked: true}
	if plan.Mode == config.ModeLegacy {
		return a, nil
	}
	var missing []RuntimeRole
	for _, candidate := range []struct {
		role     RuntimeRole
		selected bool
		factory  RuntimeFactory
	}{
		{RoleAPI, plan.API, f.API},
		{RolePersistence, plan.PersistenceConsumer, f.Persistence},
		{RoleQuarantine, plan.QuarantineConsumer, f.Quarantine},
		{RoleOutbox, plan.Outbox, f.Outbox},
	} {
		if !candidate.selected {
			continue
		}
		if candidate.factory == nil {
			missing = append(missing, candidate.role)
		} else {
			a.selected = append(a.selected, selectedRuntimeFactory{role: candidate.role, factory: candidate.factory})
		}
	}
	if len(missing) != 0 {
		return RuntimeAssembly{}, &RuntimeAssemblyError{mode: plan.Mode, missing: missing}
	}
	return a, nil
}

// ValidateServices is a second, pure gate AFTER service construction and BEFORE
// any activation. It checks an exact set of non-nil instances, including typed
// nils, without invoking lifecycle methods. The caller still owns all returned
// handles and must roll them back on failure; this function never closes them.
func (a RuntimeAssembly) ValidateServices(services map[RuntimeRole]RuntimeService) error {
	if !a.checked {
		return ErrRuntimeServices
	}
	for _, entry := range a.selected {
		service := services[entry.role]
		if nilRuntimeService(service) {
			return fmt.Errorf("%w: missing service for %s", ErrRuntimeServices, entry.role)
		}
	}
	for role := range services {
		selected := false
		for _, entry := range a.selected {
			if entry.role == role {
				selected = true
				break
			}
		}
		if !selected {
			// The supplied map may contain an arbitrary key; do not echo it.
			return fmt.Errorf("%w: unexpected service role", ErrRuntimeServices)
		}
	}
	return nil
}

func nilRuntimeService(service RuntimeService) bool {
	if service == nil {
		return true
	}
	v := reflect.ValueOf(service)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// RuntimeAssemblyError contains only validated mode and known role names.
// MissingRoles returns a copy so diagnostics cannot mutate the original error.
type RuntimeAssemblyError struct {
	mode    config.RuntimeMode
	missing []RuntimeRole
}

func (e *RuntimeAssemblyError) Mode() config.RuntimeMode { return e.mode }
func (e *RuntimeAssemblyError) MissingRoles() []RuntimeRole {
	return append([]RuntimeRole(nil), e.missing...)
}
func (e *RuntimeAssemblyError) Error() string {
	roles := make([]string, len(e.missing))
	for i, role := range e.missing {
		roles[i] = string(role)
	}
	return fmt.Sprintf("%s: %s (missing roles: %s)", ErrRuntimeNotAssembled, e.mode, strings.Join(roles, ", "))
}
func (e *RuntimeAssemblyError) Unwrap() error { return ErrRuntimeNotAssembled }
