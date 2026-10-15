package application

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"iChat/config"
)

// No lifecycle method may be invoked by either assembly gate.
type forbiddenRuntimeService struct{}

func (*forbiddenRuntimeService) Start(context.Context, context.Context) error {
	panic("assembly gate activated a service")
}
func (*forbiddenRuntimeService) StopAdmission() { panic("assembly gate stopped a service") }
func (*forbiddenRuntimeService) Drain(context.Context) error {
	panic("assembly gate drained a service")
}
func (*forbiddenRuntimeService) Close(context.Context) error {
	panic("assembly gate closed a service")
}
func (*forbiddenRuntimeService) Done() <-chan struct{} { panic("assembly gate observed a service") }
func (*forbiddenRuntimeService) Err() error            { panic("assembly gate read service state") }

func allRuntimeFactories() RuntimeFactories {
	forbidden := func(context.Context, RoleDependencies) (RuntimeService, error) {
		panic("assembly gate invoked a factory")
	}
	return RuntimeFactories{API: forbidden, Persistence: forbidden, Quarantine: forbidden, Outbox: forbidden}
}

func runtimeExpectedRoles(mode config.RuntimeMode) []RuntimeRole {
	switch mode {
	case config.ModeLegacy:
		return nil
	case config.ModeAPI:
		return []RuntimeRole{RoleAPI}
	case config.ModeWorker:
		return []RuntimeRole{RolePersistence, RoleQuarantine, RoleOutbox}
	default:
		return []RuntimeRole{RoleAPI, RolePersistence, RoleQuarantine, RoleOutbox}
	}
}

func TestRuntimeAssemblySelectsExactRolesWithoutInvokingFactories(t *testing.T) {
	for _, mode := range []config.RuntimeMode{config.ModeLegacy, config.ModeAPI, config.ModeWorker, config.ModeAll} {
		t.Run(string(mode), func(t *testing.T) {
			c := mqTestConfig(mode)
			before, global := c, config.Conf
			f := allRuntimeFactories()
			a, err := f.Check(c)
			want := runtimeExpectedRoles(mode)
			if err != nil || a.Mode() != mode || !reflect.DeepEqual(a.Roles(), want) {
				t.Fatal("wrong runtime assembly", err, a.Roles())
			}
			if len(want) != 0 {
				copy := a.Roles()
				copy[0] = "caller-mutated-role"
				if !reflect.DeepEqual(a.Roles(), want) {
					t.Fatal("caller mutated selected roles")
				}
			}
			// Selection retains the original factory values and no unselected ones.
			f.API, f.Persistence, f.Quarantine, f.Outbox = nil, nil, nil, nil
			for _, selected := range a.selected {
				if selected.factory == nil {
					t.Fatal("factory snapshot changed with source")
				}
			}
			services := map[RuntimeRole]RuntimeService{}
			for _, role := range want {
				services[role] = &forbiddenRuntimeService{}
			}
			if err := a.ValidateServices(services); err != nil {
				t.Fatal(err)
			}
			if c != before || config.Conf != global {
				t.Fatal("assembly gate mutated source/global configuration")
			}
		})
	}
}

func TestRuntimeAssemblyOnlyRequiresSelectedFactories(t *testing.T) {
	api := allRuntimeFactories().API
	for _, tc := range []struct {
		mode config.RuntimeMode
		f    RuntimeFactories
	}{
		{config.ModeLegacy, RuntimeFactories{}},
		{config.ModeAPI, RuntimeFactories{API: api}},
		{config.ModeWorker, RuntimeFactories{Persistence: api, Quarantine: api, Outbox: api}},
	} {
		a, err := tc.f.Check(mqTestConfig(tc.mode))
		if err != nil || a.Mode() != tc.mode {
			t.Fatal("unselected factory was required", tc.mode, err)
		}
	}
	// Legacy's API and optional Redis worker belong to the old bootstrap.
	c := mqTestConfig(config.ModeLegacy)
	c.LEGACY_WORKER.Enabled = true
	c.RABBITMQ = config.RabbitMQConfig{}
	if a, err := (RuntimeFactories{}).Check(c); err != nil || len(a.Roles()) != 0 {
		t.Fatal("legacy unexpectedly required modern roles", err)
	}
	if a, err := (RuntimeFactories{}).Check(config.Config{}); err != nil || a.Mode() != config.ModeLegacy {
		t.Fatal("direct zero-value configuration lost legacy compatibility", err)
	}
}

func TestRuntimeAssemblyReportsEveryMissingRole(t *testing.T) {
	for _, mode := range []config.RuntimeMode{config.ModeAPI, config.ModeWorker, config.ModeAll} {
		t.Run(string(mode), func(t *testing.T) {
			a, err := (RuntimeFactories{}).Check(mqTestConfig(mode))
			var missing *RuntimeAssemblyError
			if !errors.Is(err, ErrRuntimeNotAssembled) || !errors.As(err, &missing) || a.checked || a.Mode() != "" || len(a.Roles()) != 0 {
				t.Fatal("missing roles returned a partial assembly", err)
			}
			if missing.Mode() != mode || !reflect.DeepEqual(missing.MissingRoles(), runtimeExpectedRoles(mode)) {
				t.Fatal("incomplete/unstable missing-role diagnostics", err)
			}
			copy := missing.MissingRoles()
			copy[0] = "modified-error"
			if !reflect.DeepEqual(missing.MissingRoles(), runtimeExpectedRoles(mode)) {
				t.Fatal("caller mutated missing-role diagnostics")
			}
		})
	}
	// A complete API must not conceal the missing worker roles in all mode.
	f := RuntimeFactories{API: allRuntimeFactories().API}
	_, err := f.Check(mqTestConfig(config.ModeAll))
	var missing *RuntimeAssemblyError
	if !errors.As(err, &missing) || !reflect.DeepEqual(missing.MissingRoles(), runtimeExpectedRoles(config.ModeWorker)) {
		t.Fatal("partial registration hid missing worker roles", err)
	}
}

func TestRuntimeAssemblyRejectsEachMissingFactory(t *testing.T) {
	for _, role := range runtimeExpectedRoles(config.ModeAll) {
		t.Run(string(role), func(t *testing.T) {
			f := allRuntimeFactories()
			switch role {
			case RoleAPI:
				f.API = nil
			case RolePersistence:
				f.Persistence = nil
			case RoleQuarantine:
				f.Quarantine = nil
			case RoleOutbox:
				f.Outbox = nil
			}
			_, err := f.Check(mqTestConfig(config.ModeAll))
			var missing *RuntimeAssemblyError
			if !errors.As(err, &missing) || !reflect.DeepEqual(missing.MissingRoles(), []RuntimeRole{role}) {
				t.Fatal("missing required factory was accepted", err)
			}
		})
	}
}

func TestRuntimeAssemblyValidatesModeBeforeRoleAvailability(t *testing.T) {
	for _, mode := range []config.RuntimeMode{"secret-invalid-mode", config.ModeAPI, config.ModeWorker, config.ModeAll} {
		c := mqTestConfig(mode)
		c.LEGACY_WORKER.Enabled = true
		a, err := (RuntimeFactories{}).Check(c)
		if !errors.Is(err, config.ErrRuntimeConfig) || errors.Is(err, ErrRuntimeNotAssembled) || a.checked {
			t.Fatal("invalid mode/conflicting legacy worker was hidden by missing roles", err)
		}
		if strings.Contains(fmt.Sprintf("%+v", err), "secret-invalid-mode") {
			t.Fatal("invalid configuration echoed into error")
		}
	}
}

func TestRuntimeAssemblyRejectsMissingNilAndUnexpectedServices(t *testing.T) {
	a, err := allRuntimeFactories().Check(mqTestConfig(config.ModeAll))
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range a.Roles() {
		for _, name := range []string{"missing", "nil", "typed_nil"} {
			t.Run(string(role)+"_"+name, func(t *testing.T) {
				services := map[RuntimeRole]RuntimeService{}
				for _, expected := range a.Roles() {
					services[expected] = &forbiddenRuntimeService{}
				}
				switch name {
				case "missing":
					delete(services, role)
				case "nil":
					services[role] = nil
				case "typed_nil":
					services[role] = (*forbiddenRuntimeService)(nil)
				}
				before := maps.Clone(services)
				if err := a.ValidateServices(services); !errors.Is(err, ErrRuntimeServices) {
					t.Fatal("incomplete service set was accepted", err)
				}
				if !reflect.DeepEqual(services, before) {
					t.Fatal("validation changed caller-owned cleanup handles")
				}
			})
		}
	}
	for _, mode := range []config.RuntimeMode{config.ModeLegacy, config.ModeAPI, config.ModeWorker} {
		a, _ := allRuntimeFactories().Check(mqTestConfig(mode))
		services := map[RuntimeRole]RuntimeService{}
		for _, role := range a.Roles() {
			services[role] = &forbiddenRuntimeService{}
		}
		extra := RoleAPI
		if mode == config.ModeAPI {
			extra = RolePersistence
		}
		services[extra] = &forbiddenRuntimeService{}
		if err := a.ValidateServices(services); !errors.Is(err, ErrRuntimeServices) {
			t.Fatal("unselected service was accepted", mode, err)
		}
	}
	if err := a.ValidateServices(nil); !errors.Is(err, ErrRuntimeServices) {
		t.Fatal("empty service set was accepted", err)
	}
	if err := (RuntimeAssembly{}).ValidateServices(nil); !errors.Is(err, ErrRuntimeServices) {
		t.Fatal("unchecked zero-value assembly bypassed validation", err)
	}
}

func TestRuntimeAssemblyGatesPerformNoNetworkIOAndRedactInputs(t *testing.T) {
	listen := func() *net.TCPListener {
		listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		return listener
	}
	amqp, management := listen(), listen()
	for _, mode := range []config.RuntimeMode{config.ModeAPI, config.ModeWorker, config.ModeAll} {
		c := mqTestConfig(mode)
		c.RABBITMQ.URL = "amqp://user:SECRET_CREDENTIAL@" + amqp.Addr().String() + "/%2Fichat"
		c.RABBITMQ.ManagementURL = "http://" + management.Addr().String()
		c.RABBITMQ.ManagementPassword = "SECRET_CREDENTIAL"
		_, err := (RuntimeFactories{}).Check(c)
		if err == nil {
			t.Fatal("empty factory registry accepted")
		}
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if strings.Contains(fmt.Sprintf(format, err), "SECRET_CREDENTIAL") {
				t.Fatal("assembly error leaked credentials")
			}
		}
		a, err := allRuntimeFactories().Check(c)
		if err != nil {
			t.Fatal(err)
		}
		services := map[RuntimeRole]RuntimeService{}
		for _, role := range a.Roles() {
			services[role] = &forbiddenRuntimeService{}
		}
		services["SECRET_CREDENTIAL"] = &forbiddenRuntimeService{}
		if err := a.ValidateServices(services); err == nil || strings.Contains(err.Error(), "SECRET_CREDENTIAL") {
			t.Fatal("unexpected role key escaped validation", err)
		}
		delete(services, "SECRET_CREDENTIAL")
		if err := a.ValidateServices(services); err != nil {
			t.Fatal(err)
		}
	}
	for _, listener := range []*net.TCPListener{amqp, management} {
		_ = listener.SetDeadline(time.Now().Add(20 * time.Millisecond))
		conn, err := listener.Accept()
		if conn != nil {
			_ = conn.Close()
			t.Fatal("pure assembly gate opened a network connection")
		}
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatal("unexpected listener failure", err)
		}
	}
}
