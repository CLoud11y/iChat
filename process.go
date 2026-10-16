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
)

// Dependency construction is deferred until after configuration and role
// checks. Signal registration belongs to the process entry, not MQ or services.
type processDependencies struct {
	legacy        startupDependencies
	legacyOptions legacyRuntimeOptions
	runLegacy     func(context.Context, config.Config, startupDependencies, legacyRuntimeOptions) error
	factories     application.RuntimeFactories
	options       application.RuntimeOptions
	runRuntime    func(context.Context, config.Config, application.RuntimeFactories, application.RuntimeOptions) error
	notifyContext func(context.Context, ...os.Signal) (context.Context, context.CancelFunc)
}

func defaultProcessDependencies() processDependencies {
	return processDependencies{
		legacy:        defaultStartupDependencies(),
		legacyOptions: defaultLegacyRuntimeOptions(),
		runLegacy:     runLegacyRuntime,
		// No production business roles are implemented yet. Do not substitute
		// transport-only or test services for these required factories.
		factories:     application.RuntimeFactories{},
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
	assembly, err := deps.factories.Check(conf)
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
	return deps.runRuntime(ctx, conf, deps.factories, deps.options)
}
