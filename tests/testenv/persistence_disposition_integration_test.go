//go:build integration

package testenv

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging/rabbitmq"
	"iChat/workers"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestPersistenceDispositionIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	f := newPersistenceLifecycleFixture(t, ctx)
	raw, err := openPublisherConnection(t, ctx, f.broker).Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	confirms, returns := raw.NotifyPublish(make(chan amqp.Confirmation, 1)), raw.NotifyReturn(make(chan amqp.Return, 1))
	if err := raw.Confirm(false); err != nil {
		t.Fatal(err)
	}
	var rawID, targetEpoch int
	publishBad := func(t *testing.T) rabbitmq.ConsumerMessage {
		t.Helper()
		rawID++
		message := rabbitmq.ConsumerMessage{Body: []byte{0xff, 0, '{'}, Headers: amqp.Table{"trace": []byte{0, 255}, "decimal": amqp.Decimal{Scale: 2, Value: -12}},
			ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageID: fmt.Sprintf("bad-%s-%d", f.env.RunID(), rawID),
			CorrelationID: fmt.Sprintf("source-%d", rawID), Exchange: rabbitmq.CommandsExchange, RoutingKey: rabbitmq.SubmitRoutingKey}
		tag := raw.GetNextPublishSeqNo()
		if err := raw.PublishWithContext(ctx, rabbitmq.CommandsExchange, rabbitmq.SubmitRoutingKey, true, false, amqp.Publishing{
			Body: message.Body, Headers: message.Headers, ContentType: message.ContentType, DeliveryMode: message.DeliveryMode, MessageId: message.MessageID, CorrelationId: message.CorrelationID}); err != nil {
			t.Fatal(err)
		}
		select {
		case ack := <-confirms:
			if !ack.Ack || ack.DeliveryTag != tag {
				t.Fatal("fixture bad input not confirmed")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case <-returns:
			t.Fatal("fixture bad input not routed")
		default:
		}
		return message
	}
	target := func(t *testing.T, address string, timeout time.Duration) *rabbitmq.QuarantinePublisherSession {
		t.Helper()
		targetEpoch++
		o := rabbitmq.DefaultSessionOptions()
		o.Publisher.Slots = 1
		o.Publisher.ConnectionEpoch = uint64(targetEpoch)
		o.OperationTimeout = timeout
		if address == "" {
			address = f.broker.AMQPURL()
		}
		s, err := rabbitmq.DialQuarantinePublisher(ctx, address, o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
		return s
	}
	service := func(t *testing.T, pool *sql.DB, publisher workers.QuarantinePublisher) *workers.PersistenceService {
		t.Helper()
		o := workers.DefaultPersistenceOptions()
		o.QuarantinePublisher = publisher
		o.PollTimeout, o.ProbeInterval, o.ProbeTimeout = 100*time.Millisecond, 100*time.Millisecond, 300*time.Millisecond
		s, err := workers.NewPersistenceService(pool, []workers.PersistenceConsumer{{Session: f.session(t, f.broker), Prefetch: 1}}, o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(context.Background()); errors.Is(err, workers.ErrPersistenceServicePending) {
				t.Error(err)
			}
		})
		if err := s.Start(ctx, ctx); err != nil {
			t.Fatal(err)
		}
		return s
	}
	readAudit := func(t *testing.T, source rabbitmq.ConsumerMessage, code string) (rabbitmq.QuarantineEnvelope, string) {
		t.Helper()
		d := getTopologyDelivery(t, ctx, f.control, rabbitmq.QuarantineQueue)
		q, err := rabbitmq.DecodeQuarantineEnvelope(d.Body)
		if err != nil {
			t.Fatal(err)
		}
		original, err := q.Source()
		if err != nil || q.ErrorCode() != code || !bytes.Equal(original.Body, source.Body) || original.CorrelationID != source.CorrelationID || original.MessageID != source.MessageID {
			t.Fatal("audit source identity or bytes changed", err)
		}
		if code == "INVALID_DELIVERY" {
			expected, err := rabbitmq.NewQuarantineEnvelope(source, code, time.Now())
			if err != nil || expected.TransportID() != q.TransportID() {
				t.Fatal("redelivery changed transport identity", err)
			}
			if !bytes.Equal(original.Headers["trace"].([]byte), source.Headers["trace"].([]byte)) {
				t.Fatal("source binary headers changed")
			}
		}
		if d.MessageId != q.TransportID() || d.Type != rabbitmq.QuarantineMessageType {
			t.Fatal("wrong target wrapper")
		}
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		} // Inspection/cleanup; not an audit SQL transaction.
		return q, d.CorrelationId
	}
	recoverBad := func(t *testing.T, source rabbitmq.ConsumerMessage, publisher workers.QuarantinePublisher) rabbitmq.QuarantinePublishResult {
		t.Helper()
		stream, err := f.session(t, f.broker).OpenConsumer(ctx, rabbitmq.PersistQueue, 1)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { stream.Close(context.Background()) })
		d, err := stream.Next(ctx)
		if err != nil || d.Message.CorrelationID != source.CorrelationID || !d.Message.Redelivered {
			t.Fatal("original bad attempt was not redelivered", err)
		}
		repository, err := database.NewMessageRepository(f.pool)
		if err != nil {
			t.Fatal(err)
		}
		processor, err := workers.NewPersistenceProcessor(repository, workers.DefaultPersistenceRetryOptions(), publisher)
		if err != nil {
			t.Fatal(err)
		}
		r, err := processor.Handle(ctx, d)
		if err != nil || r.Disposition != workers.PersistenceQuarantined {
			t.Fatal("explicit source recovery failed", err)
		}
		if err := stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		return r.Transfer
	}
	supervise := func(t *testing.T, publisher workers.QuarantinePublisher) (*workers.PersistenceSupervisor, *persistenceSupervisorSource) {
		t.Helper()
		source := &persistenceSupervisorSource{fixture: f}
		o := workers.DefaultPersistenceSupervisorOptions()
		o.Worker.QuarantinePublisher = publisher
		o.Worker.ProbeInterval, o.Worker.ProbeTimeout = 100*time.Millisecond, 300*time.Millisecond
		s, err := workers.NewPersistenceSupervisor(f.pool, source.prepare, o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(context.Background()); errors.Is(err, workers.ErrPersistenceSupervisorPending) {
				t.Error(err)
			}
		})
		if err := s.Start(ctx, ctx); err != nil {
			t.Fatal(err)
		}
		return s, source
	}
	run := func(name string, test func(*testing.T)) {
		t.Helper()
		if !t.Run(name, test) {
			t.FailNow()
		}
		f.checkEmpty(t)
		if _, ok, err := f.control.Get(rabbitmq.QuarantineQueue, false); err != nil || ok {
			t.Fatal("unaccounted audit copy", err)
		}
	}
	run("malformed_transfers_then_same_worker_persists_next_command", func(t *testing.T) {
		bad := publishBad(t)
		command, _ := f.publish(t)
		s := service(t, f.pool, target(t, "", 2*time.Second))
		readAudit(t, bad, "INVALID_DELIVERY")
		lifecycleEventually(t, func() bool { return f.count(t, command) == 1 })
		if !s.ConsumerReady() {
			t.Fatal("successful isolation stopped serial worker")
		}
		s.StopAdmission()
		if err := s.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		f.checkDurableSet(t, command)
	})
	run("actual_constraint_rollback_transfers_without_partial_business_rows", func(t *testing.T) {
		command, receipt := f.publish(t)
		var nextSeq uint64
		if err := f.pool.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM message").Scan(&nextSeq); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.ExecContext(ctx, fmt.Sprintf("ALTER TABLE message ADD CONSTRAINT chk_disposition_fixture CHECK (seq <> %d)", nextSeq)); err != nil {
			t.Fatal(err)
		}
		dropped := false
		t.Cleanup(func() {
			if !dropped {
				f.pool.ExecContext(context.Background(), "ALTER TABLE message DROP CHECK chk_disposition_fixture")
			}
		})
		scenario := newPersistenceRetrySQL("")
		pool := f.retryPool(t, scenario)
		s := service(t, pool, target(t, "", 2*time.Second))
		d := getTopologyDelivery(t, ctx, f.control, rabbitmq.QuarantineQueue)
		q, err := rabbitmq.DecodeQuarantineEnvelope(d.Body)
		if err != nil || q.ErrorCode() != "DATA_CONSTRAINT" {
			t.Fatal("actual constraint did not isolate", err)
		}
		original, err := q.Source()
		if err != nil || original.CorrelationID != receipt.PublishAttemptID || original.MessageID != command.CommandID {
			t.Fatal("constraint source changed", err)
		}
		s.StopAdmission()
		if err := s.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		var rows int
		if err := f.pool.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM message WHERE id=?)+(SELECT COUNT(*) FROM message_command WHERE command_id=?)+(SELECT COUNT(*) FROM message_outbox WHERE command_id=?)`, command.CommandID, command.CommandID, command.CommandID).Scan(&rows); err != nil || rows != 0 || scenario.begins.Load() != 1 || scenario.rollbacks.Load() != 1 || scenario.commits.Load() != 0 {
			t.Fatal("constraint left partial data or retried", err)
		}
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.ExecContext(ctx, "ALTER TABLE message DROP CHECK chk_disposition_fixture"); err != nil {
			t.Fatal(err)
		}
		dropped = true
	})
	run("target_return_stops_supervision_and_preserves_source", func(t *testing.T) {
		s, source := supervise(t, target(t, "", 2*time.Second))
		if err := f.control.QueueUnbind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			f.control.QueueBind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, false, nil)
		})
		bad := publishBad(t)
		lifecycleAwait(t, s.Done())
		if workers.ClassifyPersistenceFailure(s.Err()) != workers.FailureQuarantine || s.Status().State != workers.PersistenceStateFailed || source.calls.Load() != 1 || source.closed.Load() != 1 {
			t.Fatal("target failure caused a DB recovery/requeue loop", s.Err())
		}
		if err := f.control.QueueBind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, false, nil); err != nil {
			t.Fatal(err)
		}
		recoverBad(t, bad, target(t, "", 2*time.Second))
		readAudit(t, bad, "INVALID_DELIVERY")
	})
	run("lost_target_confirm_retains_source_and_duplicate_audit_identity", func(t *testing.T) {
		bad := publishBad(t)
		proxy := startSessionProxy(t, f.broker.Address)
		broker := f.broker
		broker.Address = proxy.address
		publisher := target(t, broker.AMQPURL(), 250*time.Millisecond)
		proxy.mode.Store(1)
		s, source := supervise(t, publisher)
		lifecycleAwait(t, s.Done())
		if workers.ClassifyPersistenceFailure(s.Err()) != workers.FailureQuarantine || source.calls.Load() != 1 {
			t.Fatal("lost target confirmation was not retained", s.Err())
		}
		select {
		case <-proxy.dropped:
		default:
			t.Fatal("actual confirm was not dropped")
		}
		first, attempt := readAudit(t, bad, "INVALID_DELIVERY")
		r := recoverBad(t, bad, target(t, "", 2*time.Second))
		second, again := readAudit(t, bad, "INVALID_DELIVERY")
		if first.TransportID() != second.TransportID() || r.TransportID != first.TransportID() || attempt == again {
			t.Fatal("duplicate isolation changed transport identity or reused attempt")
		}
		if err := publisher.Close(ctx); err != nil {
			t.Fatal(err)
		}
		proxy.Close()
	})
	run("lost_source_ack_after_target_confirmation_replays_same_transport", func(t *testing.T) {
		bad := publishBad(t)
		publisher := target(t, "", 2*time.Second)
		proxy := startSessionProxy(t, f.broker.Address)
		proxy.dropConsumerAcks.Store(true)
		broker := f.broker
		broker.Address = proxy.address
		stream, err := f.session(t, broker).OpenConsumer(ctx, rabbitmq.PersistQueue, 1)
		if err != nil {
			t.Fatal(err)
		}
		d, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		repository, err := database.NewMessageRepository(f.pool)
		if err != nil {
			t.Fatal(err)
		}
		processor, err := workers.NewPersistenceProcessor(repository, workers.DefaultPersistenceRetryOptions(), publisher)
		if err != nil {
			t.Fatal(err)
		}
		r, err := processor.Handle(ctx, d)
		if err != nil || r.Disposition != workers.PersistenceQuarantined {
			t.Fatal("local source ACK write failed", err)
		}
		lifecycleEventually(t, func() bool { return proxy.consumerAcksDropped.Load() == 1 })
		if err := stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		proxy.Close()
		first, attempt := readAudit(t, bad, "INVALID_DELIVERY")
		recoverBad(t, bad, publisher)
		second, again := readAudit(t, bad, "INVALID_DELIVERY")
		if first.TransportID() != second.TransportID() || attempt == again {
			t.Fatal("ACK loss changed transport identity")
		}
	})
	run("actual_commit_success_with_unknown_response_never_isolates", func(t *testing.T) {
		command, receipt := f.publish(t)
		scenario := newPersistenceRetrySQL("")
		scenario.commitError = io.EOF
		s := service(t, f.retryPool(t, scenario), target(t, "", 2*time.Second))
		lifecycleAwait(t, s.Done())
		if workers.ClassifyPersistenceFailure(s.Err()) != workers.FailureCommitUnknown || scenario.commits.Load() != 1 {
			t.Fatal("commit unknown lost priority", s.Err())
		}
		if _, ok, err := f.control.Get(rabbitmq.QuarantineQueue, false); err != nil || ok {
			t.Fatal("commit unknown was quarantined", err)
		}
		f.recover(t, receipt, database.CommandReplayed)
		f.checkDurableSet(t, command)
	})
	var gaps int
	if err := f.pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation c WHERE c.last_seq<>(SELECT COUNT(*) FROM message m WHERE m.conversation_id=c.id) OR c.last_seq<>(SELECT COALESCE(MAX(m.seq),0) FROM message m WHERE m.conversation_id=c.id)`).Scan(&gaps); err != nil || gaps != 0 {
		t.Fatal("disposition left sequence gaps", err)
	}
}
