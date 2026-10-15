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
	factories     application.RuntimeFactories
	options       application.RuntimeOptions
	runRuntime    func(context.Context, config.Config, application.RuntimeFactories, application.RuntimeOptions) error
	notifyContext func(context.Context, ...os.Signal) (context.Context, context.CancelFunc)
}

func defaultProcessDependencies() processDependencies {
	return processDependencies{
		legacy: defaultStartupDependencies(),
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
	if assembly.Mode() == config.ModeLegacy {
		// Legacy still owns blocking Gin HTTP and hijacked WebSockets. Until
		// their stop/join adapter exists, retain the OS's signal behavior;
		// swallowing an interrupt here would leave the old server running.
		return bootstrapLegacy(conf, deps.legacy)
	}
	if deps.runRuntime == nil || deps.notifyContext == nil {
		return errors.New("process: missing modern runtime dependency")
	}
	// Pure MQ validation precedes process-wide signal registration as well as
	// all external I/O. RunRuntime revalidates its own inputs and owns cleanup.
	if _, err := conf.MQRuntime(); err != nil {
		return err
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
	// Return every error, including shutdown failures joined with cancellation.
	// Only RunRuntime can classify cancellation after successful activation as
	// a clean stop. The entry must not suppress errors.Is(context.Canceled).
	return deps.runRuntime(ctx, conf, deps.factories, deps.options)
}
