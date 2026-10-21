package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"iChat/application"
	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
)

type adminReplayStore struct {
	detail database.QuarantineDetail
	events *[]string
	stage  string
}

func (s *adminReplayStore) Get(context.Context, string) (database.QuarantineDetail, error) {
	*s.events = append(*s.events, "read")
	if s.stage == "query" {
		return database.QuarantineDetail{}, database.ErrQuarantineInvariant
	}
	return s.detail, nil
}
func (s *adminReplayStore) RecordReplay(_ context.Context, expected database.QuarantineDetail, receipt rabbitmq.PublishResult) error {
	*s.events = append(*s.events, "record")
	if expected.Input.Envelope().TransportID() != s.detail.Summary.TransportID || receipt.Outcome != rabbitmq.PublishAccepted {
		return errors.New("wrong record arguments")
	}
	if s.stage == "sql" {
		return database.ErrQuarantineReplayChanged
	}
	if s.stage == "sql_commit" {
		return database.ErrQuarantineCommitUnknown
	}
	return nil
}

type adminReplayPublisher struct {
	store *adminReplayStore
	t     *testing.T
}

func (p *adminReplayPublisher) Publish(_ context.Context, command messaging.Command, stamp time.Time) (rabbitmq.PublishResult, error) {
	*p.store.events = append(*p.store.events, "publish")
	source, _ := p.store.detail.Input.Envelope().Source()
	wire, _ := messaging.DecodeEnvelope(source.Body)
	want, _ := messaging.ValidateEnvelope(wire)
	if command != want || stamp.Format(time.RFC3339Nano) != wire.ReceivedAt {
		p.t.Fatal("replay changed command or original reception time")
	}
	r := rabbitmq.PublishResult{Outcome: rabbitmq.PublishAccepted, CommandID: command.CommandID, PublishAttemptID: "171c6c89-bfa6-4bd1-a738-cfa7d7a63164", ConnectionEpoch: 1, ChannelEpoch: 1, DeliveryTag: 1}
	switch p.store.stage {
	case "unknown":
		r.Outcome = rabbitmq.PublishUnknown
	case "nack":
		r.Outcome = rabbitmq.PublishNacked
	case "return":
		r.Outcome = rabbitmq.PublishUnroutable
	case "publish_error":
		return r, errors.New("private secret transport error")
	case "bad_receipt":
		r.CommandID = "wrong"
	}
	return r, nil
}
func (p *adminReplayPublisher) Close(ctx context.Context) error {
	*p.store.events = append(*p.store.events, "close publisher")
	if ctx.Err() != nil {
		p.t.Fatal("cleanup inherited canceled operation context")
	}
	return nil
}

func TestQuarantineReplayCLI(t *testing.T) {
	for _, stage := range []string{"success", "unknown", "nack", "return", "publish_error", "bad_receipt", "sql", "sql_commit", "query", "resolved", "unidentified", "previous_changed", "pending_confirmed", "dial", "output"} {
		t.Run(stage, func(t *testing.T) {
			input := adminQuarantineInput(t, "replay-private-source", stage != "unidentified", false)
			var events []string
			id := input.Envelope().TransportID()
			store := &adminReplayStore{detail: database.QuarantineDetail{Summary: database.QuarantineSummary{TransportID: id, State: "open"}, Input: input}, stage: stage, events: &events}
			args := []string{"--transport-id", id, "--confirm"}
			if stage == "resolved" {
				store.detail.Summary.State = "resolved"
			}
			if stage == "previous_changed" || stage == "pending_confirmed" {
				previous := "a4e3044c-5f44-4d1b-9d61-a35ab19ec01d"
				store.detail.Summary.State, store.detail.Summary.ReplayAttemptID = "replay_pending", &previous
				if stage == "pending_confirmed" {
					args = append(args, "--expect-replay-attempt", previous)
				}
			}
			var output bytes.Buffer
			var writer io.Writer = &output
			if stage == "output" {
				writer = adminBrokenWriter{}
			}
			err := runQuarantineReplay(args, writer, func(string) (application.QuarantineReplayStore, func() error, error) {
				return store, func() error { events = append(events, "close SQL"); return nil }, nil
			}, func(context.Context, string) (quarantineReplayPublisher, error) {
				events = append(events, "dial")
				if stage == "dial" {
					return nil, errors.New("secret credentials")
				}
				return &adminReplayPublisher{store, t}, nil
			})
			success := stage == "success" || stage == "pending_confirmed"
			if (err == nil) != success || (err != nil && strings.Contains(err.Error(), "secret")) {
				t.Fatal("wrong outcome or error leak", events, err)
			}
			preflight := stage == "query" || stage == "resolved" || stage == "unidentified" || stage == "previous_changed"
			if preflight {
				if output.Len() != 0 || !reflect.DeepEqual(events, []string{"read", "close SQL"}) {
					t.Fatal("invalid record opened MQ", events)
				}
				return
			}
			if stage != "output" {
				var result application.QuarantineReplayResult
				if json.Unmarshal(output.Bytes(), &result) != nil || result.CommandID == "" || bytes.Contains(output.Bytes(), []byte("private-source")) {
					t.Fatal("lost reconciliation identity or leaked source")
				}
				if success && result.AttemptRecord != "recorded" {
					t.Fatal("confirmed result not recorded")
				}
				if (stage == "sql" || stage == "sql_commit") && (result.AttemptRecord != "unconfirmed" || result.Publish != rabbitmq.PublishAccepted || result.AttemptID == "") {
					t.Fatal("confirmed publication hidden by SQL error")
				}
			}
			recorded := strings.Contains(strings.Join(events, ","), "record")
			if recorded != (success || stage == "sql" || stage == "sql_commit" || stage == "output") {
				t.Fatal("unconfirmed publication changed SQL", events)
			}
			if success && !reflect.DeepEqual(events, []string{"read", "dial", "publish", "record", "close publisher", "close SQL"}) {
				t.Fatal("record/owner ordering changed", events)
			}
		})
	}
}

func TestQuarantineReplayCLIRequiresExactConfirmation(t *testing.T) {
	id := "mq:v1:" + strings.Repeat("1", 64)
	for _, args := range [][]string{nil, {"--confirm"}, {"--transport-id", id}, {"--transport-id", "secret", "--confirm"}, {"--transport-id", id, "--confirm=false"}, {"--transport-id", id, "--confirm", "--expect-replay-attempt", "secret"}, {"--transport-id", id, "--confirm", "extra"}, {"--transport-id", id, "--confirm", "--unknown=secret"}} {
		called := false
		err := runQuarantineReplay(args, io.Discard, func(string) (application.QuarantineReplayStore, func() error, error) {
			called = true
			return nil, nil, nil
		}, nil)
		if called || err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid replay opened storage or leaked input", err)
		}
	}
	if err := run([]string{"quarantine", "replay", "--transport-id", id}, io.Discard); err == nil {
		t.Fatal("main dispatch bypassed confirmation")
	}
}
