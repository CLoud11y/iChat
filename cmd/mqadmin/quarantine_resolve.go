package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"iChat/database"
)

const quarantineResolveUsage = "usage: mqadmin quarantine resolve --transport-id id --confirm [--expect-replay-attempt uuid] [--config path]"

type quarantineResolver interface {
	Resolve(context.Context, database.QuarantineResolveSelection) (database.QuarantineResolveResult, error)
}

type quarantineResolveOpen func(string) (quarantineResolver, func() error, error)

func runQuarantineResolve(args []string, output io.Writer, open quarantineResolveOpen) (err error) {
	flags := flag.NewFlagSet("quarantine resolve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configFile := os.Getenv("ICHAT_CONFIG_FILE")
	if configFile == "" {
		configFile = "config/config.yml"
	}
	file := flags.String("config", configFile, "SQL configuration; empty uses process environment only")
	id := flags.String("transport-id", "", "exact audit transport ID")
	confirm := flags.Bool("confirm", false, "resolve only after verified canonical business terminal")
	previous := flags.String("expect-replay-attempt", "", "exact currently recorded replay attempt, if any")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !*confirm || !validReplaySelection(*id, *previous) {
		return errors.New(quarantineResolveUsage)
	}
	resolver, closeResolver, err := open(*file)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeResolver()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, resolveErr := resolver.Resolve(ctx, database.QuarantineResolveSelection{TransportID: *id, PreviousAttempt: *previous})
	if result.TransportID != "" {
		if json.NewEncoder(output).Encode(result) != nil {
			return errors.Join(resolveErr, errors.New("write quarantine resolution output failed"))
		}
	}
	return resolveErr
}

func openQuarantineResolver(file string) (quarantineResolver, func() error, error) {
	reader, closeReader, err := openQuarantineReader(file)
	if err != nil {
		return nil, nil, err
	}
	return reader.(quarantineResolver), closeReader, nil
}
