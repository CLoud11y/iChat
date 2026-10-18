package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func persistenceMessage(t *testing.T) (messaging.Command, rabbitmq.ConsumerMessage) {
	t.Helper()
	command, err := messaging.Normalize(12, messaging.SendRequest{
		ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private,
		TargetID: "34", Media: messaging.Text, Content: "private body 中🙂\x00",
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := messaging.NewEnvelope(command, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return command, rabbitmq.ConsumerMessage{
		Body: body, ContentType: "application/json", DeliveryMode: 2,
		MessageID: command.CommandID, CorrelationID: envelope.PublishAttemptID,
		Exchange: rabbitmq.CommandsExchange, RoutingKey: rabbitmq.SubmitRoutingKey,
	}
}

func persistenceExecution(command messaging.Command, state database.CommandState) database.CommandExecution {
	sender, _ := strconv.ParseUint(command.SenderID, 10, 64)
	r := &database.CommandResult{
		SenderID: sender, ClientMessageID: command.ClientMessageID, CommandID: command.CommandID,
		PayloadHash: command.PayloadHash, State: state, CreatedAt: time.Now(), CompletedAt: time.Now(),
	}
	if state == database.CommandPersisted {
		r.MessageID, r.ConversationID = command.CommandID, command.ConversationID
	} else {
		r.ReasonCode = "NOT_FRIENDS"
	}
	return database.CommandExecution{Disposition: database.CommandApplied, Result: r}
}

func requirePersistenceFailure(t *testing.T, execution database.CommandExecution, err error, kind PersistenceFailure) {
	t.Helper()
	var failure *PersistenceError
	if !errors.As(err, &failure) || failure.Kind != kind || !reflect.DeepEqual(execution, database.CommandExecution{}) {
		t.Fatalf("expected empty execution and %s, got %+v / %v", kind, execution, err)
	}
}

func TestPersistenceWireValidationPreservesResponsibility(t *testing.T) {
	_, message := persistenceMessage(t)
	for _, tc := range []struct {
		name string
		edit func(*rabbitmq.ConsumerMessage)
	}{
		{"invalid_json", func(m *rabbitmq.ConsumerMessage) { m.Body = []byte("broken private body") }},
		{"oversized", func(m *rabbitmq.ConsumerMessage) { m.Body = bytes.Repeat([]byte(" "), messaging.MaxBodyBytes+1) }},
		{"invalid_utf8", func(m *rabbitmq.ConsumerMessage) { m.Body = append([]byte{0xff}, m.Body...) }},
		{"duplicate_field", func(m *rabbitmq.ConsumerMessage) { m.Body = append([]byte(`{"schemaVersion":1,`), m.Body[1:]...) }},
		{"unknown_field", func(m *rabbitmq.ConsumerMessage) { m.Body = append([]byte(`{"unexpected":true,`), m.Body[1:]...) }},
		{"trailing_object", func(m *rabbitmq.ConsumerMessage) { m.Body = append(bytes.Clone(m.Body), []byte("{}")...) }},
		{"content_type", func(m *rabbitmq.ConsumerMessage) { m.ContentType = "text/plain" }},
		{"encoding", func(m *rabbitmq.ConsumerMessage) { m.ContentEncoding = "gzip" }},
		{"transient", func(m *rabbitmq.ConsumerMessage) { m.DeliveryMode = 1 }},
		{"missing_message_id", func(m *rabbitmq.ConsumerMessage) { m.MessageID = "" }},
		{"wrong_message_id", func(m *rabbitmq.ConsumerMessage) { m.MessageID = strings.Repeat("a", 64) }},
		{"missing_attempt", func(m *rabbitmq.ConsumerMessage) { m.CorrelationID = "" }},
		{"wrong_attempt", func(m *rabbitmq.ConsumerMessage) { m.CorrelationID = "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1" }},
		{"quarantine_route", func(m *rabbitmq.ConsumerMessage) { m.Exchange = rabbitmq.QuarantineExchange }},
		{"wrong_route", func(m *rabbitmq.ConsumerMessage) { m.RoutingKey = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := message
			m.Body = bytes.Clone(message.Body)
			tc.edit(&m)
			assertInvalidPersistenceMessage(t, m)
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*messaging.Envelope)
	}{
		{"version", func(e *messaging.Envelope) { e.SchemaVersion++ }},
		{"hash", func(e *messaging.Envelope) { e.PayloadHash = strings.Repeat("0", 64) }},
		{"command_id", func(e *messaging.Envelope) { e.CommandID = strings.Repeat("0", 64) }},
		{"sender", func(e *messaging.Envelope) { e.SenderID = "99" }},
		{"content", func(e *messaging.Envelope) { e.Content = "altered content" }},
		{"noncanonical_key", func(e *messaging.Envelope) { e.ClientMessageID = strings.ToUpper(e.ClientMessageID) }},
		{"attempt", func(e *messaging.Envelope) { e.PublishAttemptID = "invalid" }},
		{"received_at", func(e *messaging.Envelope) { e.ReceivedAt = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := messaging.DecodeEnvelope(message.Body)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(&e)
			m := message
			m.Body, err = json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			assertInvalidPersistenceMessage(t, m)
		})
	}
}

func assertInvalidPersistenceMessage(t *testing.T, message rabbitmq.ConsumerMessage) {
	t.Helper()
	before := bytes.Clone(message.Body)
	h := &PersistenceHandler{persist: func(context.Context, messaging.Command) (database.CommandExecution, error) {
		t.Error("invalid wire reached database")
		return database.CommandExecution{}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	execution, err := h.handle(ctx, message, func(context.Context) error { t.Error("invalid wire ACKed"); return nil })
	requirePersistenceFailure(t, execution, err, InvalidDelivery)
	if !bytes.Equal(before, message.Body) {
		t.Fatal("raw body was modified")
	}
}

func TestPersistenceTerminalResultsAckOnce(t *testing.T) {
	command, message := persistenceMessage(t)
	for _, disposition := range []database.CommandDisposition{database.CommandApplied, database.CommandReplayed, database.CommandConflict} {
		for _, state := range []database.CommandState{database.CommandPersisted, database.CommandRejected} {
			t.Run(string(disposition)+"/"+string(state), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				expected := persistenceExecution(command, state)
				expected.Disposition = disposition
				if disposition == database.CommandConflict {
					expected.Result = nil
				}
				calls, acks := 0, 0
				h := &PersistenceHandler{persist: func(gotCtx context.Context, got messaging.Command) (database.CommandExecution, error) {
					calls++
					if gotCtx != ctx || !reflect.DeepEqual(got, command) {
						t.Fatal("canonical command or bounded context changed")
					}
					return expected, nil
				}}
				// Broker redelivery metadata cannot choose or skip business execution.
				message.Redelivered = true
				actual, err := h.handle(ctx, message, func(gotCtx context.Context) error {
					acks++
					if calls != 1 || gotCtx != ctx {
						t.Fatal("ACK preceded repository completion")
					}
					return nil
				})
				if err != nil || calls != 1 || acks != 1 || !reflect.DeepEqual(actual, expected) {
					t.Fatalf("handoff failed: %v (calls=%d acks=%d)", err, calls, acks)
				}
			})
		}
	}
}

func TestPersistenceFailuresNeverAckOrRetry(t *testing.T) {
	command, message := persistenceMessage(t)
	dbFailure := errors.New("SQL password=secret private body")
	for _, tc := range []struct {
		name string
		err  error
		kind PersistenceFailure
	}{
		{"database", dbFailure, PersistenceFailed},
		{"commit_unknown", errors.Join(database.ErrCommandCommitUnknown, dbFailure), CommitUnknown},
		{"cancelled", context.Canceled, PersistenceFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			calls := 0
			h := &PersistenceHandler{persist: func(context.Context, messaging.Command) (database.CommandExecution, error) {
				calls++
				return persistenceExecution(command, database.CommandPersisted), tc.err
			}}
			execution, err := h.handle(ctx, message, func(context.Context) error { t.Error("failed transaction ACKed"); return nil })
			requirePersistenceFailure(t, execution, err, tc.kind)
			if calls != 1 || !errors.Is(err, tc.err) {
				t.Fatal("error lost or persistence retried")
			}
			for _, format := range []string{"%v", "%+v", "%#v"} {
				if strings.Contains(fmt.Sprintf(format, err), "secret") {
					t.Fatal("ordinary logging exposes raw database diagnostics")
				}
			}
		})
	}
}

func TestPersistenceIncompleteResultsRetainDelivery(t *testing.T) {
	command, message := persistenceMessage(t)
	for _, tc := range []struct {
		name string
		edit func(*database.CommandExecution)
	}{
		{"zero", func(e *database.CommandExecution) { *e = database.CommandExecution{} }},
		{"no_result", func(e *database.CommandExecution) { e.Result = nil }},
		{"wrong_key", func(e *database.CommandExecution) { e.Result.ClientMessageID = "other" }},
		{"wrong_sender", func(e *database.CommandExecution) { e.Result.SenderID++ }},
		{"wrong_command", func(e *database.CommandExecution) { e.Result.CommandID = "other" }},
		{"wrong_hash", func(e *database.CommandExecution) { e.Result.PayloadHash = "other" }},
		{"wrong_message", func(e *database.CommandExecution) { e.Result.MessageID = "other" }},
		{"wrong_conversation", func(e *database.CommandExecution) { e.Result.ConversationID = "other" }},
		{"partial", func(e *database.CommandExecution) { e.Result.CompletedAt = time.Time{} }},
		{"bad_state", func(e *database.CommandExecution) { e.Result.State = "pending" }},
		{"conflict_leaks_result", func(e *database.CommandExecution) { e.Disposition = database.CommandConflict }},
		{"rejection_claim", func(e *database.CommandExecution) {
			e.Result.State, e.Result.MessageID, e.Result.ConversationID, e.Result.ReasonCode = database.CommandRejected, "", "", "__claim"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			bad := persistenceExecution(command, database.CommandPersisted)
			tc.edit(&bad)
			h := &PersistenceHandler{persist: func(context.Context, messaging.Command) (database.CommandExecution, error) { return bad, nil }}
			execution, err := h.handle(ctx, message, func(context.Context) error { t.Error("partial result ACKed"); return nil })
			requirePersistenceFailure(t, execution, err, InvalidTerminalResult)
		})
	}
}

func TestPersistenceWaitsForRepositoryAndRetainsAckFailure(t *testing.T) {
	command, message := persistenceMessage(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	acked, done := make(chan struct{}), make(chan error, 1)
	ackErr := rabbitmq.ErrConsumerDispositionUnknown
	h := &PersistenceHandler{persist: func(context.Context, messaging.Command) (database.CommandExecution, error) {
		close(entered)
		<-release
		return persistenceExecution(command, database.CommandPersisted), nil
	}}
	go func() {
		execution, err := h.handle(ctx, message, func(context.Context) error { close(acked); return ackErr })
		if !reflect.DeepEqual(execution, database.CommandExecution{}) {
			done <- errors.New("ambiguous ACK returned a successful execution")
			return
		}
		done <- err
	}()
	<-entered
	select {
	case <-acked:
		t.Error("ACK ran while repository was still in progress")
	default:
	}
	close(release)
	select {
	case err := <-done:
		requirePersistenceFailure(t, database.CommandExecution{}, err, AcknowledgementFailed)
		if !errors.Is(err, ackErr) {
			t.Fatal("lost ACK uncertainty")
		}
	case <-ctx.Done():
		t.Fatal("handler did not finish")
	}
}

func TestPersistenceContextAndNilInputs(t *testing.T) {
	command, message := persistenceMessage(t)
	if h, err := NewPersistenceHandler(nil); h != nil || !errors.Is(err, ErrPersistenceInput) {
		t.Fatal("nil repository accepted")
	}
	h := &PersistenceHandler{persist: func(context.Context, messaging.Command) (database.CommandExecution, error) {
		t.Error("invalid input reached repository")
		return database.CommandExecution{}, nil
	}}
	cancelled, cancel := context.WithTimeout(context.Background(), time.Second)
	cancel()
	for _, ctx := range []context.Context{nil, context.Background(), cancelled} {
		if _, err := h.handle(ctx, message, func(context.Context) error { t.Error("invalid input ACKed"); return nil }); err == nil {
			t.Fatal("invalid context accepted")
		}
	}
	if _, err := h.Handle(context.Background(), nil); !errors.Is(err, ErrPersistenceInput) {
		t.Fatal("nil delivery accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var missing *PersistenceHandler
	if _, err := missing.handle(ctx, message, func(context.Context) error { return nil }); !errors.Is(err, ErrPersistenceInput) {
		t.Fatal("nil handler accepted")
	}
	h.persist = func(context.Context, messaging.Command) (database.CommandExecution, error) {
		cancel()
		return persistenceExecution(command, database.CommandPersisted), nil
	}
	execution, err := h.handle(ctx, message, func(context.Context) error { t.Error("cancelled post-commit ACK attempted"); return nil })
	requirePersistenceFailure(t, execution, err, AcknowledgementFailed)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("post-commit cancellation lost")
	}
}
