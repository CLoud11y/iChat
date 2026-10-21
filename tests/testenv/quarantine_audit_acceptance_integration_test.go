//go:build integration

package testenv

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iChat/application"
	"iChat/database"
	"iChat/health"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/workers"

	mysql "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
)

type auditAcceptanceTarget struct {
	base              *rabbitmq.QuarantinePublisherSession
	published, active atomic.Int64
}

func (p *auditAcceptanceTarget) Publish(ctx context.Context, q rabbitmq.QuarantineEnvelope) (rabbitmq.QuarantinePublishResult, error) {
	p.active.Add(1)
	defer p.active.Add(-1)
	result, err := p.base.Publish(ctx, q)
	if err == nil {
		p.published.Add(1)
	}
	return result, err
}

type auditAcceptanceRuntime struct {
	audit         *workers.QuarantineSupervisor
	persistence   *workers.PersistenceSupervisor
	inputs        application.RoleDependencies
	persistInputs application.RoleDependencies
	stop          context.CancelFunc
	done          chan struct{}
	err           error
}

func auditAcceptanceStart(t *testing.T, f *persistenceLifecycleFixture, pool *sql.DB, target workers.QuarantinePublisher, dual bool, address string, o application.RuntimeOptions, allowError bool) *auditAcceptanceRuntime {
	t.Helper()
	qo := workers.DefaultQuarantineSupervisorOptions()
	qo.Worker.PollTimeout, qo.Worker.ProbeInterval, qo.Worker.ProbeTimeout = 100*time.Millisecond, 100*time.Millisecond, 300*time.Millisecond
	qo.BaseDelay, qo.MaxDelay, qo.SetupTimeout = 200*time.Millisecond, 800*time.Millisecond, 8*time.Second
	qfactory, err := application.NewSupervisedQuarantineFactory(pool, qo)
	if err != nil {
		t.Fatal(err)
	}
	po := workers.DefaultPersistenceSupervisorOptions()
	po.Worker.PollTimeout, po.Worker.ProbeInterval, po.Worker.ProbeTimeout = 100*time.Millisecond, 100*time.Millisecond, 300*time.Millisecond
	po.BaseDelay, po.MaxDelay, po.SetupTimeout = 200*time.Millisecond, 800*time.Millisecond, 8*time.Second
	po.Worker.QuarantinePublisher = target
	pfactory, err := application.NewSupervisedPersistenceFactory(pool, po)
	if err != nil {
		t.Fatal(err)
	}
	r := &auditAcceptanceRuntime{done: make(chan struct{})}
	captured := make(chan struct{}, 1)
	factories := application.RuntimeFactories{
		Persistence: func(ctx context.Context, in application.RoleDependencies) (application.RuntimeService, error) {
			r.persistInputs = in
			if !dual {
				return persistenceFixtureCompanion(ctx, in)
			}
			service, err := pfactory(ctx, in)
			if err == nil {
				r.persistence = service.(*workers.PersistenceSupervisor)
			}
			return service, err
		},
		Quarantine: func(ctx context.Context, in application.RoleDependencies) (application.RuntimeService, error) {
			service, err := qfactory(ctx, in)
			if err == nil {
				r.audit, r.inputs = service.(*workers.QuarantineSupervisor), in
				captured <- struct{}{}
			}
			return service, err
		},
		Outbox: persistenceFixtureCompanion,
	}
	conf := persistenceRuntimeConfig(f)
	if address != "" {
		broker := f.broker
		broker.Address = address
		conf.RABBITMQ.URL = broker.AMQPURL()
	}
	process, stop := context.WithCancel(f.ctx)
	r.stop = stop
	go func() { r.err = application.RunRuntime(process, conf, factories, o); close(r.done) }()
	t.Cleanup(func() {
		stop()
		select {
		case <-r.done:
			if r.err != nil && !allowError {
				t.Error("audit acceptance runtime failed", r.err)
			}
		case <-time.After(20 * time.Second):
			t.Error("audit acceptance runtime did not return")
		}
		if r.audit != nil {
			select {
			case <-r.audit.Done():
			case <-time.After(20 * time.Second):
				t.Error("audit owner still active")
			}
		}
		if r.persistence != nil {
			select {
			case <-r.persistence.Done():
			case <-time.After(20 * time.Second):
				t.Error("persistence owner still active")
			}
		}
	})
	select {
	case <-captured:
	case <-r.done:
		t.Fatal("audit role construction failed", r.err)
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	lifecycleEventually(t, func() bool {
		return r.inputs.Health.RuntimeReadiness().Consumers == health.Ready || persistenceSupervisorDone(r.done)
	})
	if persistenceSupervisorDone(r.done) && !allowError {
		t.Fatal("audit activation failed", r.err)
	}
	return r
}
func (r *auditAcceptanceRuntime) close(t *testing.T) {
	t.Helper()
	r.stop()
	lifecycleAwait(t, r.done)
	if r.err != nil {
		t.Fatal(r.err)
	}
	lifecycleAwait(t, r.audit.Done())
	if r.persistence != nil {
		lifecycleAwait(t, r.persistence.Done())
	}
}

func TestQuarantineAuditAcceptanceIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	f := newPersistenceLifecycleFixture(t, ctx)
	settings := f.dsn.Clone()
	settings.InterpolateParams = true
	base, err := mysql.NewConnector(settings)
	if err != nil {
		t.Fatal(err)
	}
	sharedConnector := &quarantineAuditFaultConnector{base: base}
	shared := sql.OpenDB(sharedConnector)
	t.Cleanup(func() { shared.Close() })
	f.pool = shared
	baseTarget, err := rabbitmq.DialQuarantinePublisher(ctx, f.broker.AMQPURL(), rabbitmq.DefaultSessionOptions())
	if err != nil {
		t.Fatal(err)
	}
	target := &auditAcceptanceTarget{base: baseTarget}
	t.Cleanup(func() {
		if target.active.Load() != 0 {
			t.Error("borrowed quarantine publisher still in use")
		}
		if err := baseTarget.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	confirms, returns := f.control.NotifyPublish(make(chan amqp.Confirmation, 1)), f.control.NotifyReturn(make(chan amqp.Return, 1))
	if err := f.control.Confirm(false); err != nil {
		t.Fatal(err)
	}
	known := map[string]rabbitmq.QuarantineEnvelope{}
	business := map[string]messaging.Command{}
	serial, mainACKs, dlxCopies := 0, 0, 0
	newInput := func(t *testing.T) rabbitmq.QuarantineEnvelope {
		t.Helper()
		serial++
		q, err := rabbitmq.NewQuarantineEnvelope(rabbitmq.ConsumerMessage{Body: []byte{0xff, 0, '{'}, ContentType: "application/json", DeliveryMode: 2, MessageID: fmt.Sprintf("audit-acceptance-%s-%d", f.env.RunID(), serial), CorrelationID: fmt.Sprintf("physical-%d", serial), Exchange: rabbitmq.CommandsExchange, RoutingKey: rabbitmq.SubmitRoutingKey, Headers: amqp.Table{"binary": []byte{0, 255}, "large": int64(9007199254740993), "decimal": amqp.Decimal{Scale: 2, Value: -12}}}, "INVALID_DELIVERY", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		known[q.TransportID()] = q
		return q
	}
	put := func(t *testing.T, q rabbitmq.QuarantineEnvelope) {
		t.Helper()
		if _, err := target.Publish(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	publishSource := func(t *testing.T, q rabbitmq.QuarantineEnvelope) {
		t.Helper()
		source, err := q.Source()
		if err != nil {
			t.Fatal(err)
		}
		tag := f.control.GetNextPublishSeqNo()
		if err := f.control.PublishWithContext(ctx, rabbitmq.CommandsExchange, rabbitmq.SubmitRoutingKey, true, false, amqp.Publishing{Body: source.Body, Headers: source.Headers, ContentType: source.ContentType, DeliveryMode: source.DeliveryMode, MessageId: source.MessageID, CorrelationId: source.CorrelationID}); err != nil {
			t.Fatal(err)
		}
		select {
		case c := <-confirms:
			if !c.Ack || c.DeliveryTag != tag {
				t.Fatal("fixture source not confirmed")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case <-returns:
			t.Fatal("fixture source unrouted")
		default:
		}
	}
	auditCount := func(q rabbitmq.QuarantineEnvelope) int {
		var n int
		if shared.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_quarantine WHERE transport_id=?", q.TransportID()).Scan(&n) != nil {
			return -1
		}
		return n
	}
	document := func(t *testing.T, q rabbitmq.QuarantineEnvelope) []byte {
		t.Helper()
		var data []byte
		if err := shared.QueryRowContext(ctx, "SELECT raw_headers FROM message_quarantine WHERE transport_id=?", q.TransportID()).Scan(&data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	awaitAudit := func(t *testing.T, q rabbitmq.QuarantineEnvelope) {
		t.Helper()
		lifecycleEventually(t, func() bool { return auditCount(q) == 1 })
	}
	queueState := func(queue string) (int, int, int) {
		var state struct {
			Ready   int               `json:"messages_ready"`
			Unacked int               `json:"messages_unacknowledged"`
			Stats   struct{ Ack int } `json:"message_stats"`
		}
		_, err := f.env.rabbitAPI(ctx, http.MethodGet, "/api/queues/"+url.PathEscape(f.broker.VHost)+"/"+queue, nil, &state)
		if err != nil {
			return -1, -1, -1
		}
		return state.Ready, state.Unacked, state.Stats.Ack
	}
	awaitEmpty := func(t *testing.T) {
		t.Helper()
		lifecycleEventually(t, func() bool {
			pr, pu, pa := queueState(rabbitmq.PersistQueue)
			qr, qu, qa := queueState(rabbitmq.QuarantineQueue)
			return pr == 0 && pu == 0 && qr == 0 && qu == 0 && pa >= mainACKs && int64(qa) >= target.published.Load()+int64(dlxCopies) && target.active.Load() == 0
		})
	}
	connections := func() int {
		var records []struct{ VHost string }
		if _, err := f.env.rabbitAPI(ctx, http.MethodGet, "/api/connections", nil, &records); err != nil {
			return -1
		}
		n := 0
		for _, record := range records {
			if record.VHost == f.broker.VHost {
				n++
			}
		}
		return n
	}
	lifecycleEventually(t, func() bool { return connections() == 3 })
	checkSet := func(t *testing.T) {
		t.Helper()
		rows, err := shared.QueryContext(ctx, "SELECT transport_id,raw_body,raw_headers,sender_id,client_message_id,command_id,payload_hash,state,replay_attempt_id,resolved_at FROM message_quarantine")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		seen := map[string]bool{}
		for rows.Next() {
			var id, state string
			var body, data []byte
			var sender, client, command, hash, replay, resolved sql.NullString
			if err := rows.Scan(&id, &body, &data, &sender, &client, &command, &hash, &state, &replay, &resolved); err != nil {
				t.Fatal(err)
			}
			want, ok := known[id]
			if !ok || seen[id] {
				t.Fatal("unexpected audit identity")
			}
			seen[id] = true
			input, err := rabbitmq.DecodeQuarantineAudit(data)
			if err != nil {
				t.Fatal("stored audit invalid", err)
			}
			a, _ := input.Envelope().StableContent()
			b, _ := want.StableContent()
			source, _ := want.Source()
			if input.Envelope().TransportID() != id || !bytes.Equal(a, b) || !bytes.Equal(body, source.Body) || sender.Valid || client.Valid || command.Valid || hash.Valid || state != "open" || replay.Valid || resolved.Valid {
				t.Fatal("audit set/content/state mismatch", id)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(seen) != len(known) {
			t.Fatal("missing audit identity", len(seen), len(known))
		}
		for _, command := range business {
			f.checkDurableSet(t, command)
		}
		var messages, outbox, commands int
		if err := shared.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM message),(SELECT COUNT(*) FROM message_command),(SELECT COUNT(*) FROM message_outbox)").Scan(&messages, &commands, &outbox); err != nil || messages != len(business) || commands != len(business) || outbox != 2*len(business) {
			t.Fatal("audit created duplicate business effects", messages, commands, outbox, err)
		}
	}
	run := func(name string, test func(*testing.T)) {
		if !t.Run(name, test) {
			t.FailNow()
		}
		awaitEmpty(t)
		for _, queue := range []string{rabbitmq.PersistQueue, rabbitmq.QuarantineQueue} {
			if _, ok, err := f.control.Get(queue, false); err != nil || ok {
				t.Fatal("queue not empty after recovery", queue, err)
			}
		}
		lifecycleEventually(t, func() bool { return connections() == 3 })
		checkSet(t)
	}
	inspectRedelivery := func(t *testing.T, q rabbitmq.QuarantineEnvelope, expectedBody ...[]byte) {
		t.Helper()
		body := q.Encode()
		if len(expectedBody) == 1 {
			body = expectedBody[0]
		}
		d := getTopologyDelivery(t, ctx, f.control, rabbitmq.QuarantineQueue)
		if !d.Redelivered || !bytes.Equal(d.Body, body) || d.MessageId != q.TransportID() {
			t.Fatal("original quarantine delivery lost")
		}
		if err := d.Nack(false, true); err != nil {
			t.Fatal(err)
		}
	}
	recover := func(t *testing.T) {
		t.Helper()
		r := auditAcceptanceStart(t, f, shared, target, false, "", application.DefaultRuntimeOptions(), false)
		awaitEmpty(t)
		r.close(t)
	}
	faultPool := func(t *testing.T, stage string) (*sql.DB, *quarantineAuditFaultConnector, func()) {
		t.Helper()
		settings := f.dsn.Clone()
		settings.InterpolateParams = true
		base, err := mysql.NewConnector(settings)
		if err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		fault := &quarantineAuditFaultConnector{base: base, stage: stage, entered: make(chan context.Context, 1), release: release}
		pool := sql.OpenDB(fault)
		t.Cleanup(func() { pool.Close() })
		var once sync.Once
		return pool, fault, func() { once.Do(func() { close(release) }) }
	}
	run("committed_audit_response_error_recovers_original_identity", func(t *testing.T) {
		pool, fault, release := faultPool(t, "commit_error")
		defer release()
		r := auditAcceptanceStart(t, f, pool, target, false, "", application.DefaultRuntimeOptions(), false)
		fault.armed.Store(true)
		q := newInput(t)
		put(t, q)
		select {
		case <-fault.entered:
		case <-r.done:
			t.Fatal(r.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		awaitAudit(t, q)
		lifecycleEventually(t, func() bool { return r.audit.Status().Generation >= 2 && r.audit.ConsumerReady() })
		if r.audit.Status().LastFailure != workers.FailureCommitUnknown {
			t.Fatal("commit uncertainty classification lost", r.audit.Status())
		}
		awaitEmpty(t)
		r.close(t)
	})
	run("real_sql_commit_ok_packet_loss_recovers_committed_audit", func(t *testing.T) {
		proxy := startQuarantineSQLProxy(t, f.dsn.Addr)
		settings := f.dsn.Clone()
		settings.Addr = proxy.address
		settings.TLSConfig = ""
		settings.InterpolateParams = true
		connector, err := mysql.NewConnector(settings)
		if err != nil {
			t.Fatal(err)
		}
		pool := sql.OpenDB(connector)
		t.Cleanup(func() { pool.Close() })
		r := auditAcceptanceStart(t, f, pool, target, false, "", application.DefaultRuntimeOptions(), false)
		proxy.armed.Store(true)
		q := newInput(t)
		put(t, q)
		lifecycleEventually(t, func() bool { return proxy.dropped.Load() == 1 })
		awaitAudit(t, q)
		lifecycleEventually(t, func() bool { return r.audit.Status().Generation >= 2 && r.audit.ConsumerReady() })
		if r.audit.Status().LastFailure != workers.FailureCommitUnknown {
			t.Fatal("lost COMMIT response misclassified", r.audit.Status())
		}
		awaitEmpty(t)
		r.close(t)
	})
	run("second_generation_real_ack_drop_keeps_committed_audit", func(t *testing.T) {
		proxy := startSessionProxy(t, f.broker.Address)
		proxy.dropConsumerAcks.Store(true)
		r := auditAcceptanceStart(t, f, shared, target, false, proxy.address, application.DefaultRuntimeOptions(), false)
		if err := r.inputs.Consumers[0].Session.(*rabbitmq.ConsumerSession).Close(ctx); err != nil {
			t.Fatal(err)
		}
		lifecycleEventually(t, func() bool { return r.audit.Status().Generation == 2 && r.audit.ConsumerReady() })
		q := newInput(t)
		put(t, q)
		lifecycleEventually(t, func() bool { return proxy.consumerAcksDropped.Load() == 1 })
		awaitAudit(t, q)
		before := document(t, q)
		r.close(t)
		if r.audit.Status().Generation != 2 {
			t.Fatal("local ACK triggered automatic replay")
		}
		inspectRedelivery(t, q)
		recover(t)
		if !bytes.Equal(before, document(t, q)) {
			t.Fatal("ACK-loss recovery replaced first audit snapshot")
		}
	})
	for _, stage := range []string{"before_commit", "after_commit"} {
		run("kill_process_"+stage, func(t *testing.T) {
			q := newInput(t)
			put(t, q)
			kill := quarantineAuditCrashWorker(t, f, stage)
			want := 0
			if stage == "after_commit" {
				want = 1
			}
			if auditCount(q) != want {
				t.Fatal("child barrier did not match SQL commit state", stage, auditCount(q))
			}
			var before []byte
			if want == 1 {
				before = document(t, q)
			}
			kill()
			lifecycleEventually(t, func() bool { return connections() == 3 })
			if auditCount(q) != want {
				t.Fatal("process kill changed committed audit state")
			}
			inspectRedelivery(t, q)
			recover(t)
			if before != nil && !bytes.Equal(before, document(t, q)) {
				t.Fatal("post-commit crash overwrote audit")
			}
		})
	}
	run("broker_dlx_first_observation_survives_wrapper_duplicates", func(t *testing.T) {
		q := newInput(t)
		publishSource(t, q)
		d := getTopologyDelivery(t, ctx, f.control, rabbitmq.PersistQueue)
		dlxCopies++
		if err := d.Reject(false); err != nil {
			t.Fatal(err)
		}
		r := auditAcceptanceStart(t, f, shared, target, false, "", application.DefaultRuntimeOptions(), false)
		awaitEmpty(t)
		awaitAudit(t, q)
		before := document(t, q)
		stored, err := rabbitmq.DecodeQuarantineAudit(before)
		if err != nil || stored.Format() != rabbitmq.QuarantineDeadLetter {
			t.Fatal("raw DLX observation missing", err)
		}
		source, _ := stored.Envelope().Source()
		if source.Headers["x-death"] == nil || source.Exchange != rabbitmq.QuarantineExchange {
			t.Fatal("broker metadata discarded")
		}
		put(t, q)
		put(t, q)
		awaitEmpty(t)
		r.close(t)
		if !bytes.Equal(before, document(t, q)) {
			t.Fatal("wrapper replaced first DLX audit")
		}
	})
	run("conflicting_stored_content_stops_without_ack_or_overwrite", func(t *testing.T) {
		q := newInput(t)
		put(t, q)
		session := f.session(t, f.broker)
		stream, err := session.OpenConsumer(ctx, rabbitmq.QuarantineQueue, 1)
		if err != nil {
			t.Fatal(err)
		}
		d, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		input, err := rabbitmq.ParseQuarantineInput(d.Message, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		repository, _ := database.NewQuarantineRepository(shared)
		if _, err := repository.Save(ctx, input); err != nil {
			t.Fatal(err)
		}
		before := document(t, q)
		source, _ := q.Source()
		source.MessageID += "-different-content"
		other, err := rabbitmq.NewQuarantineEnvelope(source, "INVALID_DELIVERY", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		modified := d.Message
		modified.Body, modified.MessageID = other.Encode(), other.TransportID()
		otherInput, err := rabbitmq.ParseQuarantineInput(modified, input.ReceivedAt())
		if err != nil {
			t.Fatal(err)
		}
		otherDoc, _ := otherInput.EncodeAudit()
		if _, err := shared.ExecContext(ctx, "UPDATE message_quarantine SET raw_headers=? WHERE transport_id=?", string(otherDoc), q.TransportID()); err != nil {
			t.Fatal(err)
		}
		corrupt := document(t, q)
		if err := d.Reject(ctx, true); err != nil {
			t.Fatal(err)
		}
		if err := stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		r := auditAcceptanceStart(t, f, shared, target, false, "", application.DefaultRuntimeOptions(), true)
		lifecycleAwait(t, r.done)
		lifecycleAwait(t, r.audit.Done())
		if !errors.Is(r.err, database.ErrQuarantineConflict) || r.audit.Status().Generation != 1 || !bytes.Equal(corrupt, document(t, q)) {
			t.Fatal("conflict was ACKed, retried or overwritten", r.err)
		}
		inspectRedelivery(t, q)
		// Restore only the deliberately corrupted fixture document. Production
		// recovery never deletes or replaces a canonical audit to force replay.
		if _, err := shared.ExecContext(ctx, "UPDATE message_quarantine SET raw_headers=? WHERE transport_id=?", string(before), q.TransportID()); err != nil {
			t.Fatal(err)
		}
		recover(t)
		if !bytes.Equal(before, document(t, q)) {
			t.Fatal("recovery changed restored canonical observation")
		}
	})
	run("dual_production_roles_restore_shared_storage_and_exact_sets", func(t *testing.T) {
		r := auditAcceptanceStart(t, f, shared, target, true, "", application.DefaultRuntimeOptions(), false)
		if err := f.env.SetMySQLRunning(ctx, false); err != nil {
			t.Fatal(err)
		}
		restored := false
		restart := func() {
			if err := f.env.SetMySQLRunning(ctx, true); err != nil {
				t.Fatal(err)
			}
			conf, err := f.env.Config()
			if err != nil {
				t.Fatal(err)
			}
			f.dsn = f.dsn.Clone()
			f.dsn.Addr = net.JoinHostPort(conf.MYSQL.Host, conf.MYSQL.Port)
			settings := f.dsn.Clone()
			settings.InterpolateParams = true
			base, err := mysql.NewConnector(settings)
			if err != nil {
				t.Fatal(err)
			}
			sharedConnector.mu.Lock()
			sharedConnector.base = base
			sharedConnector.mu.Unlock()
		}
		t.Cleanup(func() {
			if !restored {
				r.stop()
				restart()
			}
		})
		lifecycleEventually(t, func() bool {
			return r.audit.Status().State == workers.PersistenceStateBackoff && r.persistence.Status().State == workers.PersistenceStateBackoff
		})
		qgen, pgen := r.audit.Status().Generation, r.persistence.Status().Generation
		for i := 0; i < 3; i++ {
			command, _ := f.publish(t)
			mainACKs++
			business[command.CommandID] = command
			if _, err := f.producer.Publish(ctx, command, time.Now()); err != nil {
				t.Fatal(err)
			}
			mainACKs++
			q := newInput(t)
			publishSource(t, q)
			publishSource(t, q)
			mainACKs += 2
		}
		lifecycleEventually(t, func() bool {
			q, err := f.control.QueueInspect(rabbitmq.PersistQueue)
			return err == nil && q.Messages == 12
		})
		if r.audit.Status().Generation != qgen || r.persistence.Status().Generation != pgen || r.inputs.Health.RuntimeReadiness().Consumers == health.Ready {
			t.Fatal("offline roles consumed or reacquired sessions")
		}
		restart()
		restored = true
		lifecycleEventually(t, func() bool {
			return r.audit.Status().Generation > qgen && r.persistence.Status().Generation > pgen && r.inputs.Health.RuntimeReadiness().Consumers == health.Ready
		})
		awaitEmpty(t)
		r.close(t)
		for _, in := range append(append([]application.PreparedConsumer{}, r.inputs.Consumers...), r.persistInputs.Consumers...) {
			if !persistenceSupervisorDone(in.Session.Done()) {
				t.Fatal("retired role session leaked")
			}
		}
		if err := shared.PingContext(ctx); err != nil {
			t.Fatal("dual runtime released borrowed storage", err)
		}
	})
	run("dual_role_pending_retains_shared_pool_and_late_audit_responsibility", func(t *testing.T) {
		pool, fault, release := faultPool(t, "after_commit")
		defer release()
		o := application.DefaultRuntimeOptions()
		o.DrainTimeout, o.CloseTimeout = 100*time.Millisecond, 100*time.Millisecond
		r := auditAcceptanceStart(t, f, pool, target, true, "", o, true)
		fault.armed.Store(true)
		q := newInput(t)
		publishSource(t, q)
		mainACKs++
		var operation context.Context
		select {
		case operation = <-fault.entered:
		case <-r.done:
			t.Fatal(r.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		awaitAudit(t, q)
		command, _ := f.publish(t)
		mainACKs++
		business[command.CommandID] = command
		lifecycleEventually(t, func() bool { return f.count(t, command) == 1 })
		r.stop()
		lifecycleAwait(t, r.done)
		if r.err == nil || persistenceSupervisorDone(r.audit.Done()) || operation.Err() == nil {
			t.Fatal("pending audit lost owner", r.err)
		}
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal("shared pool closed before audit join", err)
		}
		if target.active.Load() != 0 || persistenceSupervisorDone(target.base.Done()) {
			t.Fatal("borrowed target owner released prematurely")
		}
		release()
		lifecycleAwait(t, r.audit.Done())
		lifecycleAwait(t, r.persistence.Done())
		stored, err := rabbitmq.DecodeQuarantineAudit(document(t, q))
		if err != nil {
			t.Fatal(err)
		}
		observed, err := stored.Delivery()
		if err != nil {
			t.Fatal(err)
		}
		inspectRedelivery(t, q, observed.Body)
		recovery := auditAcceptanceStart(t, f, shared, target, true, "", application.DefaultRuntimeOptions(), false)
		awaitEmpty(t)
		recovery.close(t)
		if auditCount(q) != 1 {
			t.Fatal("pending audit produced extra rows")
		}
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
	})
	if err := migrations.ApplyReliable(ctx, shared); err != nil {
		t.Fatal("schema changed", err)
	}
	if err := rabbitmq.VerifyLocal(ctx, f.management, f.broker.VHost); err != nil {
		t.Fatal(err)
	}
}
