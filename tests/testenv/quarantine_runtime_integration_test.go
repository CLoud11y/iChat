//go:build integration

package testenv

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/application"
	"iChat/database"
	"iChat/health"
	"iChat/messaging/rabbitmq"
	"iChat/workers"

	mysql "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
)

type quarantineRuntimeCall struct {
	ctx      context.Context
	snapshot rabbitmq.QuarantineEnvelope
	receipt  rabbitmq.QuarantinePublishResult
}

// The barrier follows a real successful broker publication. It models a
// borrowed callback not returning yet, not a network confirm-loss fault.
type quarantineRuntimeTarget struct {
	base    *rabbitmq.QuarantinePublisherSession
	entered chan quarantineRuntimeCall
	release <-chan struct{}
	active  atomic.Int64
	closes  atomic.Int64
	once    sync.Once
	err     error
}

func (p *quarantineRuntimeTarget) Publish(ctx context.Context, q rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error) {
	p.active.Add(1)
	defer p.active.Add(-1)
	r, err := p.base.Publish(ctx, q)
	if err == nil && p.release != nil {
		p.entered <- quarantineRuntimeCall{ctx, q, r}
		<-p.release
	}
	return r, err
}

func (p *quarantineRuntimeTarget) Close(ctx context.Context) error {
	if p.active.Load() != 0 {
		return errors.New("fixture target owner still holds active callbacks")
	}
	p.once.Do(func() { p.closes.Add(1); p.err = p.base.Close(ctx) })
	return p.err
}

func quarantineRuntimeAwait(t *testing.T, p *quarantineRuntimeTarget) quarantineRuntimeCall {
	t.Helper()
	select {
	case call := <-p.entered:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("real target publication did not reach return barrier")
	}
	return quarantineRuntimeCall{}
}

func TestQuarantineRuntimeIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	f := newPersistenceLifecycleFixture(t, ctx)
	shared := f.pool
	confirms, returns := f.control.NotifyPublish(make(chan amqp.Confirmation, 1)), f.control.NotifyReturn(make(chan amqp.Return, 1))
	if err := f.control.Confirm(false); err != nil {
		t.Fatal(err)
	}
	rawID, targetEpoch := 0, 0
	badMessage := func() rabbitmq.ConsumerMessage {
		rawID++
		return rabbitmq.ConsumerMessage{Body: []byte{0xff, 0, '{'}, Headers: amqp.Table{"trace": []byte{0, 255}, "decimal": amqp.Decimal{Scale: 2, Value: -12}},
			ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageID: fmt.Sprintf("runtime-bad-%s-%d", f.env.RunID(), rawID),
			CorrelationID: fmt.Sprintf("source-%d", rawID), Exchange: rabbitmq.CommandsExchange, RoutingKey: rabbitmq.SubmitRoutingKey}
	}
	publish := func(t *testing.T, source rabbitmq.ConsumerMessage) {
		t.Helper()
		tag := f.control.GetNextPublishSeqNo()
		if err := f.control.PublishWithContext(ctx, rabbitmq.CommandsExchange, rabbitmq.SubmitRoutingKey, true, false, amqp.Publishing{
			Body: source.Body, Headers: source.Headers, ContentType: source.ContentType, DeliveryMode: source.DeliveryMode, MessageId: source.MessageID, CorrelationId: source.CorrelationID}); err != nil {
			t.Fatal(err)
		}
		select {
		case ack := <-confirms:
			if !ack.Ack || ack.DeliveryTag != tag {
				t.Fatal("source fixture publication not confirmed")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case <-returns:
			t.Fatal("source fixture publication not routed")
		default:
		}
	}
	target := func(t *testing.T, address string, timeout time.Duration, release <-chan struct{}) *quarantineRuntimeTarget {
		t.Helper()
		targetEpoch++
		o := rabbitmq.DefaultSessionOptions()
		o.Publisher.Slots = 1
		o.Publisher.ConnectionEpoch = uint64(targetEpoch)
		o.OperationTimeout = timeout
		if address == "" {
			address = f.broker.AMQPURL()
		}
		base, err := rabbitmq.DialQuarantinePublisher(ctx, address, o)
		if err != nil {
			t.Fatal(err)
		}
		p := &quarantineRuntimeTarget{base: base, entered: make(chan quarantineRuntimeCall, 1), release: release}
		t.Cleanup(func() {
			if err := p.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
		return p
	}
	readAudit := func(t *testing.T, source rabbitmq.ConsumerMessage) string {
		t.Helper()
		d := getTopologyDelivery(t, ctx, f.control, rabbitmq.QuarantineQueue)
		q, err := rabbitmq.DecodeQuarantineEnvelope(d.Body)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := rabbitmq.NewQuarantineEnvelope(source, "INVALID_DELIVERY", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		original, err := q.Source()
		if err != nil || q.TransportID() != expected.TransportID() || d.MessageId != q.TransportID() || q.ErrorCode() != "INVALID_DELIVERY" || !bytes.Equal(original.Body, source.Body) || original.CorrelationID != source.CorrelationID || !bytes.Equal(original.Headers["trace"].([]byte), source.Headers["trace"].([]byte)) {
			t.Fatal("runtime audit snapshot/transport identity changed", err)
		}
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		} // Fixture observation only, not SQL audit.
		return d.CorrelationId
	}
	recoverSource := func(t *testing.T, source rabbitmq.ConsumerMessage, p workers.QuarantinePublisher) {
		t.Helper()
		stream, err := f.session(t, f.broker).OpenConsumer(ctx, rabbitmq.PersistQueue, 1)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { stream.Close(context.Background()) })
		d, err := stream.Next(ctx)
		if err != nil || d.Message.CorrelationID != source.CorrelationID || !d.Message.Redelivered {
			t.Fatal("runtime lost original redelivered attempt", err)
		}
		repository, err := database.NewMessageRepository(f.pool)
		if err != nil {
			t.Fatal(err)
		}
		processor, err := workers.NewPersistenceProcessor(repository, workers.DefaultPersistenceRetryOptions(), p)
		if err != nil {
			t.Fatal(err)
		}
		r, err := processor.Handle(ctx, d)
		if err != nil || r.Disposition != workers.PersistenceQuarantined {
			t.Fatal("explicit runtime recovery failed", err)
		}
		if err := stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	failed := func(t *testing.T, r *persistenceRuntimeCapture, class workers.PersistenceFailureClass) {
		t.Helper()
		lifecycleAwait(t, r.done)
		lifecycleAwait(t, r.service.Done())
		if !errors.Is(r.err, application.ErrRuntimeStopped) || workers.ClassifyPersistenceFailure(r.service.Err()) != class || r.service.Status().Generation != 1 || r.service.ConsumerReady() || r.inputs.Health.RuntimeReadiness().Consumers != health.Stopping {
			t.Fatal("runtime did not preserve failed role/source responsibility", r.err, r.service.Err())
		}
	}
	connections := func() int {
		var records []struct{ VHost string }
		if _, err := f.env.rabbitAPI(ctx, http.MethodGet, "/api/connections", nil, &records); err != nil {
			return -1
		}
		count := 0
		for _, record := range records {
			if record.VHost == f.broker.VHost {
				count++
			}
		}
		return count
	}
	lifecycleEventually(t, func() bool { return connections() == 2 })
	run := func(name string, test func(*testing.T)) {
		t.Helper()
		if !t.Run(name, test) {
			t.FailNow()
		}
		f.pool = shared
		f.checkEmpty(t)
		if _, ok, err := f.control.Get(rabbitmq.QuarantineQueue, false); err != nil || ok {
			t.Fatal("runtime left an unaccounted isolation copy", err)
		}
		lifecycleEventually(t, func() bool { return connections() == 2 })
	}
	run("replaced_source_gracefully_drains_confirmed_target_callback", func(t *testing.T) {
		release := make(chan struct{})
		var once sync.Once
		finish := func() { once.Do(func() { close(release) }) }
		defer finish()
		p := target(t, "", 2*time.Second, release)
		r := f.supervisedRuntimeWithOptions(t, "", application.DefaultRuntimeOptions(), false, p)
		shutdownReplaced(t, r, ctx)
		source := badMessage()
		publish(t, source)
		call := quarantineRuntimeAwait(t, p)
		r.stop()
		short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
		err := r.service.Drain(short)
		stop()
		if !errors.Is(err, workers.ErrPersistenceSupervisorPending) || call.ctx.Err() != nil || p.active.Load() != 1 || p.closes.Load() != 0 {
			t.Fatal("graceful drain abandoned borrowed target", err)
		}
		finish()
		r.close(t)
		if r.service.Status().Generation != 2 || p.active.Load() != 0 || p.closes.Load() != 0 {
			t.Fatal("drain revived generation or closed borrowed target")
		}
		if err := shared.PingContext(ctx); err != nil {
			t.Fatal(err)
		}
		readAudit(t, source)
		if err := p.Close(ctx); err != nil || p.closes.Load() != 1 {
			t.Fatal("external target owner failed to close after join", err)
		}
	})
	run("pending_runtime_retains_target_and_sql_until_late_callback_joins", func(t *testing.T) {
		base, err := mysql.NewConnector(f.dsn)
		if err != nil {
			t.Fatal(err)
		}
		pool := sql.OpenDB(base)
		t.Cleanup(func() { pool.Close() })
		f.pool = pool
		release := make(chan struct{})
		var once sync.Once
		finish := func() { once.Do(func() { close(release) }) }
		defer finish()
		p := target(t, "", 2*time.Second, release)
		o := application.DefaultRuntimeOptions()
		o.DrainTimeout, o.CloseTimeout = 150*time.Millisecond, 150*time.Millisecond
		r := f.supervisedRuntimeWithOptions(t, "", o, true, p)
		shutdownReplaced(t, r, ctx)
		source := badMessage()
		publish(t, source)
		call := quarantineRuntimeAwait(t, p)
		r.stop()
		lifecycleAwait(t, r.done)
		if !errors.Is(r.err, application.ErrRuntimeShutdown) || !errors.Is(r.err, workers.ErrPersistenceSupervisorPending) || persistenceSupervisorDone(r.service.Done()) || p.active.Load() != 1 || p.closes.Load() != 0 || call.ctx.Err() == nil {
			t.Fatal("observer expiry released unfinished transfer", r.err)
		}
		select {
		case <-p.base.Done():
			t.Fatal("runtime closed borrowed target")
		default:
		}
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal("runtime released borrowed SQL", err)
		}
		finish()
		lifecycleAwait(t, r.service.Done())
		if workers.ClassifyPersistenceFailure(r.service.Err()) != workers.FailureAcknowledgement || r.service.Status().Generation != 2 || p.active.Load() != 0 || r.service.ConsumerReady() {
			t.Fatal("late target confirmation revived stopped ACK/recovery", r.service.Err())
		}
		if err := p.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		if pool.PingContext(ctx) == nil {
			t.Fatal("explicit pool owner did not release SQL")
		}
		f.pool = shared
		old := readAudit(t, source)
		recoverSource(t, source, target(t, "", 2*time.Second, nil))
		again := readAudit(t, source)
		if old == again {
			t.Fatal("explicit recovery reused target attempt")
		}
	})
	run("actual_target_confirm_blackhole_exits_runtime_with_original_source", func(t *testing.T) {
		proxy := startSessionProxy(t, f.broker.Address)
		broker := f.broker
		broker.Address = proxy.address
		p := target(t, broker.AMQPURL(), 250*time.Millisecond, nil)
		r := f.supervisedRuntimeWithOptions(t, "", application.DefaultRuntimeOptions(), true, p)
		proxy.mode.Store(1)
		source := badMessage()
		publish(t, source)
		failed(t, r, workers.FailureQuarantine)
		select {
		case <-proxy.dropped:
		default:
			t.Fatal("actual target confirm was not intercepted")
		}
		old := readAudit(t, source)
		recoverSource(t, source, target(t, "", 2*time.Second, nil))
		again := readAudit(t, source)
		if old == again || proxy.accepted.Load() != 1 {
			t.Fatal("unknown publication reconnected/reused physical attempt")
		}
		proxy.Close()
	})
	run("target_return_is_not_hidden_by_runtime_cleanup", func(t *testing.T) {
		p := target(t, "", 2*time.Second, nil)
		r := f.supervisedRuntimeWithOptions(t, "", application.DefaultRuntimeOptions(), true, p)
		if err := f.control.QueueUnbind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			f.control.QueueBind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, false, nil)
		})
		source := badMessage()
		publish(t, source)
		failed(t, r, workers.FailureQuarantine)
		if !errors.Is(r.err, rabbitmq.ErrPublisherReturn) {
			t.Fatal("runtime lost known target Return", r.err)
		}
		if _, ok, err := f.control.Get(rabbitmq.QuarantineQueue, false); err != nil || ok {
			t.Fatal("unroutable target left a copy", err)
		}
		if err := f.control.QueueBind(rabbitmq.QuarantineQueue, rabbitmq.QuarantineRoutingKey, rabbitmq.QuarantineExchange, false, nil); err != nil {
			t.Fatal(err)
		}
		recoverSource(t, source, target(t, "", 2*time.Second, nil))
		readAudit(t, source)
	})
	run("target_capacity_nack_keeps_original_source_and_confirmed_backlog", func(t *testing.T) {
		p := target(t, "", 2*time.Second, nil)
		r := f.supervisedRuntimeWithOptions(t, "", application.DefaultRuntimeOptions(), true, p)
		filler := target(t, "", 2*time.Second, nil)
		fill := badMessage()
		snapshot, err := rabbitmq.NewQuarantineEnvelope(fill, "INVALID_DELIVERY", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if receipt, err := filler.Publish(ctx, snapshot); err != nil || receipt.Outcome != rabbitmq.PublishAccepted {
			t.Fatal("target fixture fill not confirmed", err)
		}
		confirmed := []rabbitmq.ConsumerMessage{fill}
		path := "/api/operator-policies/" + url.PathEscape(f.broker.VHost) + "/ichat.test.runtime.capacity"
		if _, err := f.env.rabbitAPI(ctx, http.MethodPut, path, map[string]any{"pattern": "^" + regexp.QuoteMeta(rabbitmq.QuarantineQueue) + "$", "apply-to": "quorum_queues", "priority": 100, "definition": map[string]any{"max-length": 1}}, nil); err != nil {
			t.Fatal(err)
		}
		removed := false
		t.Cleanup(func() {
			if !removed {
				f.env.rabbitAPI(ctx, http.MethodDelete, path, nil, nil)
			}
		})
		lifecycleEventually(t, func() bool {
			var info struct {
				Effective map[string]any `json:"effective_policy_definition"`
			}
			_, err := f.env.rabbitAPI(ctx, http.MethodGet, "/api/queues/"+url.PathEscape(f.broker.VHost)+"/"+url.PathEscape(rabbitmq.QuarantineQueue), nil, &info)
			return err == nil && info.Effective["max-length"] == float64(1)
		})
		// Observe actual reject-publish first. A newly effective quorum limit
		// does not imply that the very next publication must be rejected.
		nacked := false
		for i := 0; i < 8; i++ {
			probe := badMessage()
			q, err := rabbitmq.NewQuarantineEnvelope(probe, "INVALID_DELIVERY", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := filler.Publish(ctx, q)
			if errors.Is(err, rabbitmq.ErrPublisherNack) {
				nacked = true
				break
			}
			if err != nil || receipt.Outcome != rabbitmq.PublishAccepted {
				t.Fatal("unexpected capacity probe result", receipt, err)
			}
			confirmed = append(confirmed, probe)
		}
		if !nacked {
			t.Fatal("capacity fault never produced a real NACK")
		}
		source := badMessage()
		publish(t, source)
		failed(t, r, workers.FailureQuarantine)
		if !errors.Is(r.err, rabbitmq.ErrPublisherNack) {
			t.Fatal("target capacity not classified as NACK", r.err)
		}
		for _, message := range confirmed {
			readAudit(t, message)
		}
		if _, err := f.env.rabbitAPI(ctx, http.MethodDelete, path, nil, nil); err != nil {
			t.Fatal(err)
		}
		removed = true
		lifecycleEventually(t, func() bool { return rabbitmq.VerifyLocal(ctx, f.management, f.broker.VHost) == nil })
		recoverSource(t, source, target(t, "", 2*time.Second, nil))
		readAudit(t, source)
	})
	run("source_connection_loss_after_target_confirm_cannot_ack_a_new_generation", func(t *testing.T) {
		release := make(chan struct{})
		var once sync.Once
		finish := func() { once.Do(func() { close(release) }) }
		defer finish()
		p := target(t, "", 2*time.Second, release)
		r := f.supervisedRuntimeWithOptions(t, "", application.DefaultRuntimeOptions(), true, p)
		source := badMessage()
		publish(t, source)
		quarantineRuntimeAwait(t, p)
		for _, consumer := range r.inputs.Consumers {
			if err := consumer.Session.(*rabbitmq.ConsumerSession).Close(ctx); err != nil {
				t.Fatal(err)
			}
		}
		lifecycleEventually(t, func() bool { return r.service.Status().State == workers.PersistenceStateRetiring })
		if r.service.Status().Generation != 1 || p.active.Load() != 1 || p.closes.Load() != 0 {
			t.Fatal("replacement overtook target/source responsibility")
		}
		finish()
		failed(t, r, workers.FailureAcknowledgement)
		old := readAudit(t, source)
		recoverSource(t, source, target(t, "", 2*time.Second, nil))
		again := readAudit(t, source)
		if old == again {
			t.Fatal("source retirement lost target attempt distinction")
		}
	})
	run("real_source_ack_loss_after_replacement_and_orderly_runtime_exit", func(t *testing.T) {
		proxy := startSessionProxy(t, f.broker.Address)
		proxy.dropConsumerAcks.Store(true)
		p := target(t, "", 2*time.Second, nil)
		r := f.supervisedRuntimeWithOptions(t, proxy.address, application.DefaultRuntimeOptions(), false, p)
		shutdownReplaced(t, r, ctx)
		source := badMessage()
		publish(t, source)
		lifecycleEventually(t, func() bool { return proxy.consumerAcksDropped.Load() == 1 })
		r.close(t)
		if r.service.Status().Generation != 2 {
			t.Fatal("local ACK success triggered automatic replay")
		}
		old := readAudit(t, source)
		recoverSource(t, source, p)
		again := readAudit(t, source)
		if old == again {
			t.Fatal("redelivered source reused target attempt")
		}
		proxy.Close()
	})
	run("broken_idle_target_does_not_get_misclassified_as_database_recovery", func(t *testing.T) {
		proxy := startSessionProxy(t, f.broker.Address)
		broker := f.broker
		broker.Address = proxy.address
		p := target(t, broker.AMQPURL(), 2*time.Second, nil)
		r := f.supervisedRuntimeWithOptions(t, "", application.DefaultRuntimeOptions(), true, p)
		proxy.Close()
		lifecycleAwait(t, p.base.Done())
		source := badMessage()
		publish(t, source)
		failed(t, r, workers.FailureQuarantine)
		if !errors.Is(r.err, rabbitmq.ErrPublisherUnavailable) {
			t.Fatal("target prewrite unavailability lost", r.err)
		}
		recoverSource(t, source, target(t, "", 2*time.Second, nil))
		readAudit(t, source)
	})
	run("automatic_dead_letter_raw_delivery_retains_active_transport_identity", func(t *testing.T) {
		source := badMessage()
		publish(t, source)
		stream, err := f.session(t, f.broker).OpenConsumer(ctx, rabbitmq.PersistQueue, 1)
		if err != nil {
			t.Fatal(err)
		}
		d, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Reject(ctx, false); err != nil {
			t.Fatal(err)
		} // Fixture induces real broker DLX, no application ACK.
		if err := stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		dead := getTopologyDelivery(t, ctx, f.control, rabbitmq.QuarantineQueue)
		if !bytes.Equal(dead.Body, source.Body) || dead.Type == rabbitmq.QuarantineMessageType || dead.Headers["x-death"] == nil {
			t.Fatal("broker DLX raw form was not preserved")
		}
		message := rabbitmq.ConsumerMessage{Body: dead.Body, Headers: dead.Headers, ContentType: dead.ContentType, ContentEncoding: dead.ContentEncoding,
			DeliveryMode: dead.DeliveryMode, Priority: dead.Priority, CorrelationID: dead.CorrelationId, ReplyTo: dead.ReplyTo, Expiration: dead.Expiration,
			MessageID: dead.MessageId, Timestamp: dead.Timestamp, Type: dead.Type, UserID: dead.UserId, AppID: dead.AppId,
			ConsumerTag: dead.ConsumerTag, DeliveryTag: dead.DeliveryTag, Redelivered: dead.Redelivered, Exchange: dead.Exchange, RoutingKey: dead.RoutingKey}
		before, err := rabbitmq.NewQuarantineEnvelope(source, "INVALID_DELIVERY", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		after, err := rabbitmq.NewQuarantineEnvelope(message, "INVALID_DELIVERY", time.Now())
		if err != nil || before.TransportID() != after.TransportID() {
			t.Fatal("real DLX changed stable transport identity", err)
		}
		if err := dead.Ack(false); err != nil {
			t.Fatal(err)
		}
	})
	var rows int
	if err := shared.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM message)+(SELECT COUNT(*) FROM message_command)+(SELECT COUNT(*) FROM message_outbox)+(SELECT COUNT(*) FROM message_quarantine)`).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("technical quarantine created business/audit SQL rows", err)
	}
	if err := rabbitmq.VerifyLocal(ctx, f.management, f.broker.VHost); err != nil {
		t.Fatal(err)
	}
}
