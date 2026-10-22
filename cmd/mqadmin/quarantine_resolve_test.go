package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"iChat/database"
)

type adminResolveStore struct {
	selection database.QuarantineResolveSelection
	result    database.QuarantineResolveResult
	err       error
}

func (s *adminResolveStore) Resolve(ctx context.Context, selection database.QuarantineResolveSelection) (database.QuarantineResolveResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		return database.QuarantineResolveResult{}, errors.New("missing resolution deadline")
	}
	s.selection = selection
	return s.result, s.err
}

func TestQuarantineResolveCLI(t *testing.T) {
	id := "mq:v1:" + strings.Repeat("1", 64)
	previous := "171c6c89-bfa6-4bd1-a738-cfa7d7a63164"
	stamp := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	for _, outcome := range []string{"resolved", "already_resolved", "unconfirmed", "pending", "output", "close"} {
		t.Run(outcome, func(t *testing.T) {
			s := &adminResolveStore{result: database.QuarantineResolveResult{TransportID: id, TerminalState: database.CommandRejected, ReasonCode: database.ReasonNotFriends, Outcome: outcome, ResolvedAt: &stamp}}
			if outcome == "unconfirmed" {
				s.err, s.result.ResolvedAt = database.ErrQuarantineCommitUnknown, nil
			}
			if outcome == "pending" {
				s.err, s.result = database.ErrQuarantineTerminalPending, database.QuarantineResolveResult{}
			}
			closed := 0
			var output bytes.Buffer
			var writer io.Writer = &output
			if outcome == "output" {
				writer = adminBrokenWriter{}
			}
			err := runQuarantineResolve([]string{"--transport-id", id, "--confirm", "--expect-replay-attempt", previous}, writer, func(string) (quarantineResolver, func() error, error) {
				return s, func() error {
					closed++
					if outcome == "close" {
						return errors.New("close failed")
					}
					return nil
				}, nil
			})
			if (err == nil) != (outcome == "resolved" || outcome == "already_resolved") || closed != 1 || s.selection.TransportID != id || s.selection.PreviousAttempt != previous {
				t.Fatal("selection/result/ownership lost", err)
			}
			if outcome == "pending" && output.Len() != 0 {
				t.Fatal("pending terminal reported resolution")
			}
			if outcome == "unconfirmed" {
				var got database.QuarantineResolveResult
				if json.Unmarshal(output.Bytes(), &got) != nil || got.Outcome != "unconfirmed" || got.ResolvedAt != nil || !errors.Is(err, database.ErrQuarantineCommitUnknown) {
					t.Fatal("commit uncertainty hidden", err)
				}
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("output error leaked")
			}
		})
	}
}

func TestQuarantineResolveCLIRequiresExactConfirmation(t *testing.T) {
	id := "mq:v1:" + strings.Repeat("1", 64)
	for _, args := range [][]string{nil, {"--confirm"}, {"--transport-id", id}, {"--transport-id", id, "--confirm=false"}, {"--transport-id", "secret", "--confirm"}, {"--transport-id", id, "--confirm", "--expect-replay-attempt", "secret"}, {"--transport-id", id, "--confirm", "extra"}, {"--transport-id", id, "--confirm", "--unknown=secret"}} {
		called := false
		err := runQuarantineResolve(args, io.Discard, func(string) (quarantineResolver, func() error, error) { called = true; return nil, nil, nil })
		if called || err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid resolution opened SQL or leaked input", err)
		}
	}
	if err := run([]string{"quarantine", "resolve", "--transport-id", id}, io.Discard); err == nil {
		t.Fatal("main dispatch bypassed confirmation")
	}
}
