package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"iChat/application"
	"iChat/config"
	"iChat/workers"
)

// Dependency construction is deferred until after configuration and role
// checks. Signal registration belongs to the process entry, not MQ or services.
type processDependencies struct {
	legacy        startupDependencies
	legacyOptions legacyRuntimeOptions
	runLegacy     func(context.Context, config.Config, startupDependencies, legacyRuntimeOptions) error
	factories     application.RuntimeFactories
	api           *apiAssembly
	outbox        func(config.Config) (application.RuntimeFactory, error)
	options       application.RuntimeOptions
	runRuntime    func(context.Context, config.Config, application.RuntimeFactories, application.RuntimeOptions) error
	notifyContext func(context.Context, ...os.Signal) (context.Context, context.CancelFunc)
}

func defaultProcessDependencies() processDependencies {
	return processDependencies{
		legacy:        defaultStartupDependencies(),
		legacyOptions: defaultLegacyRuntimeOptions(),
		runLegacy:     runLegacyRuntime,
		// API and Outbox have lazy production defaults. Persistence/quarantine
		// still require explicit factories; missing roles remain a hard gate.
		factories: application.RuntimeFactories{},
		api:       &apiAssembly{options: defaultAPIOptions, dependencies: defaultAPIDependencies()},
		outbox: func(c config.Config) (application.RuntimeFactory, error) {
			return application.NewConfiguredOutboxFactory(c, workers.DefaultOutboxServiceOptions())
		},
		options:       application.DefaultRuntimeOptions(),
		runRuntime:    application.RunRuntime,
		notifyContext: signal.NotifyContext,
	}
}

func runProcess(deps processDependencies) error {
	if deps.legacy.loadConfig == nil {
		return errors.New("process: missing configuration loader")
	}
	conf, err := deps.legacy.loadConfig()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	factories := deps.factories
	var selectedAPI *apiAssembly
	var apiSettings apiOptions
	// Supply a lazy production role without constructing storage or connecting
	// before the complete role gate. Explicit caller factories take precedence.
	if (conf.RUNTIME.Mode == config.ModeWorker || conf.RUNTIME.Mode == config.ModeAll) && factories.Outbox == nil && deps.outbox != nil {
		factories.Outbox = func(ctx context.Context, inputs application.RoleDependencies) (application.RuntimeService, error) {
			factory, err := deps.outbox(conf)
			if err != nil {
				return nil, err
			}
			return factory(ctx, inputs)
		}
	}
	if (conf.RUNTIME.Mode == config.ModeAPI || conf.RUNTIME.Mode == config.ModeAll) && factories.API == nil && deps.api != nil {
		copy := *deps.api
		if copy.options == nil {
			return errAPIInput
		}
		selectedAPI = &copy
		apiSettings = copy.options(conf)
		factories.API = func(_ context.Context, inputs application.RoleDependencies) (application.RuntimeService, error) {
			return newAPIService(conf, inputs, apiSettings, copy.dependencies)
		}
	}
	assembly, err := factories.Check(conf)
	if err != nil {
		return fmt.Errorf("resolve runtime assembly: %w", err)
	}
	legacy := assembly.Mode() == config.ModeLegacy
	if deps.notifyContext == nil {
		return errors.New("process: missing signal dependency")
	}
	if legacy {
		if deps.runLegacy == nil {
			return errors.New("process: missing legacy runtime dependency")
		}
		if err := checkLegacyRuntime(conf, deps.legacy, deps.legacyOptions); err != nil {
			return err
		}
	} else {
		if deps.runRuntime == nil {
			return errors.New("process: missing modern runtime dependency")
		}
		// Pure MQ validation precedes process-wide signal registration and I/O.
		if _, err := conf.MQRuntime(); err != nil {
			return err
		}
		if selectedAPI != nil {
			if err := checkAPI(conf, apiSettings, selectedAPI.dependencies); err != nil {
				return err
			}
		}
	}
	ctx, stop := deps.notifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if stop != nil {
		// Keep registration through drain/close. A repeated signal must not
		// cancel the executor's independent cleanup contexts or cut MQ early.
		defer stop()
	}
	if ctx == nil || stop == nil {
		return errors.New("process: invalid signal context")
	}
	if legacy {
		return deps.runLegacy(ctx, conf, deps.legacy, deps.legacyOptions)
	}
	// Return every error, including shutdown failures joined with cancellation.
	// Only RunRuntime can classify cancellation after successful activation as
	// a clean stop. The entry must not suppress errors.Is(context.Canceled).
	return deps.runRuntime(ctx, conf, factories, deps.options)
}
