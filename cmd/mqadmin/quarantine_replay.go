package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"iChat/application"
	"iChat/config"
	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

const quarantineReplayUsage = "usage: mqadmin quarantine replay --transport-id id --confirm [--expect-replay-attempt uuid] [--config path]"

type quarantineReplayPublisher interface {
	Publish(context.Context, messaging.Command, time.Time) (rabbitmq.PublishResult, error)
	Close(context.Context) error
}

type quarantineReplayOpen func(string) (application.QuarantineReplayStore, func() error, error)
type quarantineReplayDial func(context.Context, string) (quarantineReplayPublisher, error)

func runQuarantineReplay(args []string, output io.Writer, open quarantineReplayOpen, dial quarantineReplayDial) (err error) {
	flags := flag.NewFlagSet("quarantine replay", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configFile := os.Getenv("ICHAT_CONFIG_FILE")
	if configFile == "" {
		configFile = "config/config.yml"
	}
	file := flags.String("config", configFile, "SQL and MQ configuration; empty uses process environment only")
	id := flags.String("transport-id", "", "exact selected audit transport ID")
	confirmed := flags.Bool("confirm", false, "explicitly publish this selected command once")
	previous := flags.String("expect-replay-attempt", "", "exact previous recorded attempt when explicitly replaying again")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !*confirmed || !validReplaySelection(*id, *previous) {
		return errors.New(quarantineReplayUsage)
	}
	store, closeStore, err := open(*file)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeStore()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var publisher quarantineReplayPublisher
	defer func() {
		if publisher != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if publisher.Close(closeCtx) != nil {
				err = errors.Join(err, errors.New("close quarantine replay publisher failed"))
			}
		}
	}()
	result, replayErr := application.ReplayQuarantine(ctx, store, application.QuarantineReplaySelection{TransportID: *id, PreviousAttempt: *previous}, func(ctx context.Context, command messaging.Command, stamp time.Time) (rabbitmq.PublishResult, error) {
		var dialErr error
		publisher, dialErr = dial(ctx, *file)
		if dialErr != nil {
			return rabbitmq.PublishResult{Outcome: rabbitmq.PublishNotStarted}, dialErr
		}
		return publisher.Publish(ctx, command, stamp)
	})
	// Preserve reconciliation material on publication/SQL uncertainty. Preflight
	// selection failures produce no JSON; errors and credentials never enter it.
	if result.CommandID != "" {
		if json.NewEncoder(output).Encode(result) != nil {
			return errors.Join(replayErr, errors.New("write quarantine replay output failed"))
		}
	}
	return replayErr
}

func validReplaySelection(id, previous string) bool {
	return database.ValidQuarantineTransportID(id) && (previous == "" || messaging.ValidPublishAttemptID(previous))
}

func openQuarantineReplayStore(file string) (application.QuarantineReplayStore, func() error, error) {
	reader, closeReader, err := openQuarantineReader(file)
	if err != nil {
		return nil, nil, err
	}
	return reader.(application.QuarantineReplayStore), closeReader, nil
}

func dialQuarantineReplayPublisher(ctx context.Context, file string) (quarantineReplayPublisher, error) {
	c, err := config.LoadRabbitMQFile(file)
	if err != nil {
		return nil, err
	}
	settings, err := c.Publisher()
	if err != nil {
		return nil, err
	}
	// A replay never creates or repairs topology and never uses the reconnecting
	// supervisor. One confirmed operation has its own bounded socket and slot.
	if err := rabbitmq.VerifyLocal(ctx, settings.Management, settings.VHost); err != nil {
		return nil, err
	}
	options := settings.Options.Session
	options.Publisher.Slots = 1
	return rabbitmq.DialPublisher(ctx, settings.URL, options)
}
