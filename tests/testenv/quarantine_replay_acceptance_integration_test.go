//go:build integration

package testenv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"iChat/application"
	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/workers"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestQuarantineReplayAcceptanceIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	run := func(name string, scenario func(*testing.T)) {
		if !t.Run(name, scenario) {
			t.FailNow() // Later cases share this fixture; never cascade after failure.
		}
	}
	program := buildReplayCLI(t, ctx)
	f := newPersistenceLifecycleFixture(t, ctx)
	t.Logf("quarantine replay acceptance run=%s", f.env.RunID())
	file := replayCLIConfig(t, f, "", "")
	repo, _ := database.NewQuarantineRepository(f.pool)
	commands, _ := database.NewMessageRepository(f.pool)
	type original struct {
		input   rabbitmq.QuarantineInput
		command messaging.Command
	}
	known := map[string]original{}
	seenAttempts := map[string]bool{}
	acks := 0
	newAudit := func(t *testing.T, reject, dlx bool) (database.QuarantineDetail, messaging.Command) {
		t.Helper()
		f.nextID++
		target := uint64(f.users[1].ID)
		if reject {
			target += 10000
		}
		command, err := messaging.Normalize(uint64(f.users[0].ID), messaging.SendRequest{ClientMessageID: fmt.Sprintf("8fd7c2ef-ae26-4bb5-91a5-%012d", f.nextID), TargetType: messaging.Private, TargetID: strconv.FormatUint(target, 10), Media: messaging.Text, Content: fmt.Sprintf("replay-acceptance:%d:中🙂\x00", f.nextID)})
		if err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().UTC().Truncate(time.Microsecond)
		wire, err := messaging.NewEnvelope(command, stamp)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		source := rabbitmq.ConsumerMessage{Body: body, ContentType: "application/json", DeliveryMode: amqp.Persistent, Exchange: rabbitmq.CommandsExchange, RoutingKey: rabbitmq.SubmitRoutingKey, MessageID: wire.CommandID, CorrelationID: wire.PublishAttemptID,
			Headers: amqp.Table{"binary": []byte{0, 255}, "large": int64(9007199254740993), "decimal": amqp.Decimal{Scale: 2, Value: -12}}}
		q, err := rabbitmq.NewQuarantineEnvelope(source, "DATA_CONSTRAINT", stamp)
		if err != nil {
			t.Fatal(err)
		}
		delivery := rabbitmq.ConsumerMessage{Body: q.Encode(), ContentType: rabbitmq.QuarantineContentType, Type: rabbitmq.QuarantineMessageType, DeliveryMode: amqp.Persistent, Exchange: rabbitmq.QuarantineExchange, RoutingKey: rabbitmq.QuarantineRoutingKey, MessageID: q.TransportID(), CorrelationID: "0123456789abcdef0123456789abcdef"}
		if dlx {
			delivery = source
			delivery.Exchange, delivery.RoutingKey = rabbitmq.QuarantineExchange, rabbitmq.QuarantineRoutingKey
			delivery.Headers["x-death"] = []any{amqp.Table{"queue": rabbitmq.PersistQueue, "exchange": rabbitmq.CommandsExchange, "routing-keys": []any{rabbitmq.SubmitRoutingKey}, "reason": "rejected", "count": int64(1), "time": stamp}}
			delivery.Headers["x-last-death-queue"], delivery.Headers["x-last-death-exchange"], delivery.Headers["x-last-death-reason"] = rabbitmq.PersistQueue, rabbitmq.CommandsExchange, "rejected"
		}
		input, err := rabbitmq.ParseQuarantineInput(delivery, stamp)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Save(ctx, input); err != nil {
			t.Fatal(err)
		}
		id := input.Envelope().TransportID()
		known[id] = original{input, command}
		detail, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return detail, command
	}
	replay := func(t *testing.T, id, previous, config string) (application.QuarantineReplayResult, error) {
		t.Helper()
		args := []string{"quarantine", "replay", "--transport-id", id, "--confirm"}
		if previous != "" {
			args = append(args, "--expect-replay-attempt", previous)
		}
		p := startReplayCLI(t, ctx, program, config, args...)
		var result application.QuarantineReplayResult
		err := p.wait(t, &result)
		return result, err
	}
	resolve := func(t *testing.T, id, previous, config string) (database.QuarantineResolveResult, error) {
		t.Helper()
		args := []string{"quarantine", "resolve", "--transport-id", id, "--confirm"}
		if previous != "" {
			args = append(args, "--expect-replay-attempt", previous)
		}
		p := startReplayCLI(t, ctx, program, config, args...)
		var result database.QuarantineResolveResult
		err := p.wait(t, &result)
		return result, err
	}
	read := func(t *testing.T, id, state, attempt string) database.QuarantineDetail {
		t.Helper()
		got, err := repo.Get(ctx, id)
		if err != nil || got.Summary.State != state {
			t.Fatal("unexpected durable audit state", state, err)
		}
		actual := ""
		if got.Summary.ReplayAttemptID != nil {
			actual = *got.Summary.ReplayAttemptID
		}
		if actual != attempt {
			t.Fatal("audit attempt overwritten or lost")
		}
		a, _ := known[id].input.EncodeAudit()
		b, _ := got.Input.EncodeAudit()
		if !bytes.Equal(a, b) {
			t.Fatal("first audit document changed")
		}
		return got
	}
	consume := func(t *testing.T, command messaging.Command, n int, terminal database.CommandState) {
		t.Helper()
		session := f.session(t, f.broker)
		stream, err := session.OpenConsumer(ctx, rabbitmq.PersistQueue, 1)
		if err != nil {
			t.Fatal(err)
		}
		handler, _ := workers.NewPersistenceHandler(commands)
		for i := 0; i < n; i++ {
			call, cancel := context.WithTimeout(ctx, 8*time.Second)
			d, err := stream.Next(call)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			wire, err := messaging.DecodeEnvelope(d.Message.Body)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			got, err := messaging.ValidateEnvelope(wire)
			if err != nil || got != command || d.Message.MessageID != command.CommandID || wire.PublishAttemptID != d.Message.CorrelationID || seenAttempts[wire.PublishAttemptID] {
				cancel()
				t.Fatal("unexpected physical replay identity/content", err)
			}
			seenAttempts[wire.PublishAttemptID] = true
			execution, err := handler.Handle(call, d)
			cancel()
			if err != nil || execution.Result == nil || execution.Result.State != terminal {
				t.Fatal("production handler did not commit and ACK replay", err)
			}
			acks++
		}
		if err := stream.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := session.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if terminal == database.CommandPersisted {
			f.checkDurableSet(t, command)
		} else {
			var messages, events int
			if err := f.pool.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM message WHERE id=?),(SELECT COUNT(*) FROM message_outbox WHERE command_id=?)", command.CommandID, command.CommandID).Scan(&messages, &events); err != nil || messages != 0 || events != 1 {
				t.Fatal("rejected command was redone", err)
			}
		}
	}
	finish := func(t *testing.T, id string, terminal database.CommandState) {
		t.Helper()
		detail, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		previous := ""
		if detail.Summary.ReplayAttemptID != nil {
			previous = *detail.Summary.ReplayAttemptID
		}
		result, err := resolve(t, id, previous, file)
		if err != nil || (result.Outcome != "resolved" && result.Outcome != "already_resolved") || result.TerminalState != terminal || result.ResolvedAt == nil {
			t.Fatal("reconciliation did not resolve terminal", err)
		}
		read(t, id, "resolved", previous)
	}
	// Every case returns to a clean canonical queue and connection baseline.
	check := func(t *testing.T) {
		t.Helper()
		lifecycleEventually(t, func() bool {
			var q struct {
				Ready   int `json:"messages_ready"`
				Unacked int `json:"messages_unacknowledged"`
				Stats   struct {
					Ack int `json:"ack"`
				} `json:"message_stats"`
			}
			status, err := f.env.rabbitAPI(ctx, http.MethodGet, "/api/queues/"+url.PathEscape(f.broker.VHost)+"/"+rabbitmq.PersistQueue, nil, &q)
			return err == nil && status == http.StatusOK && q.Ready == 0 && q.Unacked == 0 && q.Stats.Ack >= acks
		})
		f.checkEmpty(t)
		if _, ok, err := f.control.Get(rabbitmq.QuarantineQueue, false); err != nil || ok {
			t.Fatal("unexpected quarantine queue content", err)
		}
		lifecycleEventually(t, func() bool {
			var connections []struct {
				VHost string `json:"vhost"`
			}
			_, err := f.env.rabbitAPI(ctx, http.MethodGet, "/api/connections", nil, &connections)
			count := 0
			for _, c := range connections {
				if c.VHost == f.broker.VHost {
					count++
				}
			}
			return err == nil && count == 2
		})
		for id, original := range known {
			got, err := repo.Get(ctx, id)
			if err != nil || got.Summary.State != "resolved" {
				t.Fatal("prior audit did not remain resolved", err)
			}
			a, _ := original.input.EncodeAudit()
			b, _ := got.Input.EncodeAudit()
			if !bytes.Equal(a, b) {
				t.Fatal("prior audit mutated")
			}
		}
	}

	run("real_confirm_loss_keeps_open_and_duplicate_replay_is_idempotent", func(t *testing.T) {
		detail, command := newAudit(t, false, false)
		id := detail.Summary.TransportID
		proxy := startSessionProxy(t, f.broker.Address)
		proxy.mu.Lock()
		proxy.confirmSelected = func() { proxy.mode.Store(1) }
		proxy.mu.Unlock()
		result, err := replay(t, id, "", replayCLIConfig(t, f, "", proxy.address))
		if err == nil || result.Publish != rabbitmq.PublishUnknown || result.AttemptRecord != "not_attempted" || !messaging.ValidPublishAttemptID(result.AttemptID) {
			t.Fatal("lost confirmation was reported accepted", err)
		}
		select {
		case <-proxy.dropped:
		default:
			t.Fatal("no actual publisher ACK was dropped")
		}
		if proxy.framesDropped.Load() == 0 {
			t.Fatal("no AMQP frame loss")
		}
		read(t, id, "open", "")
		retry, err := replay(t, id, "", file)
		if err != nil || retry.AttemptRecord != "recorded" || retry.AttemptID == result.AttemptID {
			t.Fatal("explicit retry did not create independent attempt", err)
		}
		consume(t, command, 2, database.CommandPersisted)
		finish(t, id, database.CommandPersisted)
		check(t)
	})

	for _, nack := range []bool{false, true} {
		name := "actual_return"
		if nack {
			name = "actual_nack"
		}
		run(name+"_does_not_record_a_replay", func(t *testing.T) {
			detail, command := newAudit(t, false, nack)
			id := detail.Summary.TransportID
			const capacity = "ichat.test.replay-capacity"
			const fillExchange = "ichat.test.replay-capacity-fill"
			if nack {
				if _, err := f.control.QueueDeclare(capacity, true, false, false, false, amqp.Table{"x-queue-type": "quorum", "x-quorum-initial-group-size": int32(1), "x-max-length": int64(1), "x-overflow": "reject-publish"}); err != nil {
					t.Fatal(err)
				}
				// The fixture application account cannot write the default
				// exchange. Fill through an owned, separately named exchange.
				if err := f.control.ExchangeDeclare(fillExchange, "direct", true, false, false, false, nil); err != nil {
					t.Fatal(err)
				}
				if err := f.control.QueueBind(capacity, "fill", fillExchange, false, nil); err != nil {
					t.Fatal(err)
				}
				confirms := f.control.NotifyPublish(make(chan amqp.Confirmation, 1))
				if err := f.control.Confirm(false); err != nil {
					t.Fatal(err)
				}
				// Quorum accounting can lag a confirmed enqueue. Establish an
				// actual rejection barrier instead of assuming one enqueue fills it.
				rejected := false
				for i := 0; i < 128 && !rejected; i++ {
					tag := f.control.GetNextPublishSeqNo()
					if err := f.control.PublishWithContext(ctx, fillExchange, "fill", true, false, amqp.Publishing{DeliveryMode: amqp.Persistent, Body: []byte("owned capacity sentinel")}); err != nil {
						t.Fatal(err)
					}
					select {
					case c, ok := <-confirms:
						if !ok || c.DeliveryTag != tag {
							t.Fatal("capacity fixture channel failed")
						}
						rejected = !c.Ack
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				if !rejected {
					t.Fatal("capacity fixture did not reach actual NACK")
				}
			}
			proxy := startSessionProxy(t, f.broker.Address)
			hookDone := make(chan error, 1)
			proxy.mu.Lock()
			proxy.confirmSelected = func() {
				err := f.control.QueueUnbind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, nil)
				if err == nil && nack {
					err = f.control.QueueBind(capacity, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil)
				}
				hookDone <- err
			}
			proxy.mu.Unlock()
			result, err := replay(t, id, "", replayCLIConfig(t, f, "", proxy.address))
			select {
			case hookErr := <-hookDone:
				if hookErr != nil {
					t.Fatal(hookErr)
				}
			default:
				t.Fatal("topology drift hook did not run")
			}
			if nack {
				if err := f.control.QueueUnbind(capacity, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := f.control.QueueDelete(capacity, false, false, false); err != nil {
					t.Fatal(err)
				}
				if err := f.control.ExchangeDelete(fillExchange, false, false); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.control.QueueBind(rabbitmq.PersistQueue, rabbitmq.SubmitRoutingKey, rabbitmq.CommandsExchange, false, nil); err != nil {
				t.Fatal(err)
			}
			want := rabbitmq.PublishUnroutable
			if nack {
				want = rabbitmq.PublishNacked
			}
			if err == nil || result.Publish != want || result.AttemptRecord != "not_attempted" {
				t.Fatal("actual target rejection treated as accepted", err)
			}
			read(t, id, "open", "")
			f.checkEmpty(t)
			if _, err := replay(t, id, "", file); err != nil {
				t.Fatal(err)
			}
			consume(t, command, 1, database.CommandPersisted)
			finish(t, id, database.CommandPersisted)
			check(t)
		})
	}

	run("replay_commit_ok_loss_reconciles_without_blind_republish", func(t *testing.T) {
		detail, command := newAudit(t, false, true)
		id := detail.Summary.TransportID
		proxy := startQuarantineSQLProxy(t, f.dsn.Addr)
		proxy.armed.Store(true)
		result, err := replay(t, id, "", replayCLIConfig(t, f, proxy.address, ""))
		if err == nil || result.Publish != rabbitmq.PublishAccepted || result.AttemptRecord != "unconfirmed" || proxy.dropped.Load() != 1 {
			t.Fatal("real SQL commit response loss was not preserved", err)
		}
		read(t, id, "replay_pending", result.AttemptID)
		if _, err := replay(t, id, "", file); err == nil {
			t.Fatal("stale automatic replay selection accepted")
		}
		consume(t, command, 1, database.CommandPersisted)
		finish(t, id, database.CommandPersisted)
		check(t)
	})

	run("resolution_commit_ok_loss_keeps_original_resolution_timestamp", func(t *testing.T) {
		detail, command := newAudit(t, false, false)
		id := detail.Summary.TransportID
		published, err := replay(t, id, "", file)
		if err != nil {
			t.Fatal(err)
		}
		consume(t, command, 1, database.CommandPersisted)
		proxy := startQuarantineSQLProxy(t, f.dsn.Addr)
		proxy.armed.Store(true)
		result, err := resolve(t, id, published.AttemptID, replayCLIConfig(t, f, proxy.address, ""))
		if err == nil || result.Outcome != "unconfirmed" || result.ResolvedAt != nil || proxy.dropped.Load() != 1 {
			t.Fatal("resolution commit unknown hidden", err)
		}
		durable := read(t, id, "resolved", published.AttemptID)
		retry, err := resolve(t, id, published.AttemptID, file)
		if err != nil || retry.Outcome != "already_resolved" || retry.ResolvedAt == nil || !retry.ResolvedAt.Equal(*durable.Summary.ResolvedAt) {
			t.Fatal("resolution was rewritten after commit unknown", err)
		}
		check(t)
	})

	run("already_resolved_rollback_ok_loss_reports_uncertainty", func(t *testing.T) {
		detail, command := newAudit(t, false, true)
		id := detail.Summary.TransportID
		published, err := replay(t, id, "", file)
		if err != nil {
			t.Fatal(err)
		}
		consume(t, command, 1, database.CommandPersisted)
		finish(t, id, database.CommandPersisted)
		before := read(t, id, "resolved", published.AttemptID)
		proxy := startQuarantineSQLProxy(t, f.dsn.Addr)
		proxy.rollbackArmed.Store(true)
		result, err := resolve(t, id, published.AttemptID, replayCLIConfig(t, f, proxy.address, ""))
		if err == nil || result.Outcome != "unconfirmed" || result.ResolvedAt != nil || proxy.dropped.Load() != 1 {
			t.Fatal("rollback response loss was hidden", err)
		}
		after := read(t, id, "resolved", published.AttemptID)
		if !before.Summary.ResolvedAt.Equal(*after.Summary.ResolvedAt) {
			t.Fatal("read-only verification changed resolution")
		}
		finish(t, id, database.CommandPersisted)
		check(t)
	})

	// Holding only the audit row leaves the CLI's initial nonlocking Get free.
	// Processlist proves RecordReplay reached its locking read after real confirm.
	lockAudit := func(t *testing.T, id string) (func(), func(int)) {
		t.Helper()
		tx, err := f.pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var selected string
		if err := tx.QueryRowContext(ctx, "SELECT transport_id FROM message_quarantine WHERE transport_id=? FOR UPDATE", id).Scan(&selected); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		var once sync.Once
		unlock := func() { once.Do(func() { _ = tx.Rollback() }) }
		t.Cleanup(unlock)
		await := func(want int) {
			lifecycleEventually(t, func() bool {
				var n int
				err := f.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.processlist WHERE DB=? AND INFO LIKE 'SELECT transport_id,%FROM message_quarantine WHERE%FOR UPDATE'", f.dsn.DBName).Scan(&n)
				return err == nil && n == want
			})
		}
		return unlock, await
	}
	run("kill_actual_cli_after_confirm_before_audit_update", func(t *testing.T) {
		detail, command := newAudit(t, false, false)
		id := detail.Summary.TransportID
		unlock, await := lockAudit(t, id)
		p := startReplayCLI(t, ctx, program, file, "quarantine", "replay", "--transport-id", id, "--confirm")
		await(1)
		p.stop(t, true)
		unlock()
		read(t, id, "open", "")
		if _, err := replay(t, id, "", file); err != nil {
			t.Fatal(err)
		}
		consume(t, command, 2, database.CommandPersisted)
		finish(t, id, database.CommandPersisted)
		check(t)
	})

	run("two_actual_cli_replays_keep_one_latest_attempt", func(t *testing.T) {
		detail, command := newAudit(t, false, true)
		id := detail.Summary.TransportID
		unlock, await := lockAudit(t, id)
		first := startReplayCLI(t, ctx, program, file, "quarantine", "replay", "--transport-id", id, "--confirm")
		second := startReplayCLI(t, ctx, program, file, "quarantine", "replay", "--transport-id", id, "--confirm")
		await(2)
		unlock()
		var a, b application.QuarantineReplayResult
		ea, eb := first.wait(t, &a), second.wait(t, &b)
		if (ea == nil) == (eb == nil) || a.Publish != rabbitmq.PublishAccepted || b.Publish != rabbitmq.PublishAccepted || a.AttemptID == b.AttemptID {
			t.Fatal("concurrent physical attempts were not independently confirmed")
		}
		winner, loser := a, b
		if eb == nil {
			winner, loser = b, a
		}
		if winner.AttemptRecord != "recorded" || loser.AttemptRecord != "unconfirmed" {
			t.Fatal("concurrent audit conflict was overwritten")
		}
		read(t, id, "replay_pending", winner.AttemptID)
		consume(t, command, 2, database.CommandPersisted)
		finish(t, id, database.CommandPersisted)
		check(t)
	})

	run("resolve_wins_over_confirmed_late_replay_callback", func(t *testing.T) {
		detail, command := newAudit(t, false, false)
		id := detail.Summary.TransportID
		if _, err := commands.PersistCommand(ctx, command); err != nil {
			t.Fatal(err)
		}
		release, confirmed, done := make(chan struct{}), make(chan rabbitmq.PublishResult, 1), make(chan struct{})
		var once sync.Once
		resume := func() { once.Do(func() { close(release) }) }
		t.Cleanup(func() { resume(); lifecycleAwait(t, done) })
		var result application.QuarantineReplayResult
		var callErr error
		go func() {
			defer close(done)
			result, callErr = application.ReplayQuarantine(ctx, repo, application.QuarantineReplaySelection{TransportID: id}, func(ctx context.Context, c messaging.Command, stamp time.Time) (rabbitmq.PublishResult, error) {
				r, e := f.producer.Publish(ctx, c, stamp)
				confirmed <- r
				if e == nil {
					<-release
				}
				return r, e
			})
		}()
		select {
		case receipt := <-confirmed:
			if receipt.Outcome != rabbitmq.PublishAccepted {
				t.Fatal("real publication did not confirm")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		finish(t, id, database.CommandPersisted)
		before := read(t, id, "resolved", "")
		resume()
		lifecycleAwait(t, done)
		if callErr == nil || !errors.Is(callErr, database.ErrQuarantineReplayChanged) || result.Publish != rabbitmq.PublishAccepted || result.AttemptRecord != "unconfirmed" {
			t.Fatal("late replay overwrote terminal resolution", callErr)
		}
		after := read(t, id, "resolved", "")
		if !before.Summary.ResolvedAt.Equal(*after.Summary.ResolvedAt) {
			t.Fatal("late callback changed resolution timestamp")
		}
		consume(t, command, 1, database.CommandPersisted)
		check(t)
	})

	run("confirmed_publish_without_terminal_cannot_be_resolved", func(t *testing.T) {
		detail, command := newAudit(t, false, true)
		id := detail.Summary.TransportID
		published, err := replay(t, id, "", file)
		if err != nil {
			t.Fatal(err)
		}
		result, err := resolve(t, id, published.AttemptID, file)
		if err == nil || result.TransportID != "" {
			t.Fatal("publisher acceptance masqueraded as business terminal")
		}
		read(t, id, "replay_pending", published.AttemptID)
		consume(t, command, 1, database.CommandPersisted)
		finish(t, id, database.CommandPersisted)
		check(t)
	})

	run("business_rejection_survives_confirmed_replays", func(t *testing.T) {
		detail, command := newAudit(t, true, false)
		id := detail.Summary.TransportID
		first, err := replay(t, id, "", file)
		if err != nil {
			t.Fatal(err)
		}
		consume(t, command, 1, database.CommandRejected)
		second, err := replay(t, id, first.AttemptID, file)
		if err != nil || second.AttemptID == first.AttemptID {
			t.Fatal(err)
		}
		consume(t, command, 1, database.CommandRejected)
		result, err := resolve(t, id, second.AttemptID, file)
		if err != nil || result.TerminalState != database.CommandRejected || result.ReasonCode != database.ReasonTargetNotFound {
			t.Fatal("replay changed original business rejection", err)
		}
		check(t)
	})

	run("sql_endpoint_unavailable_does_not_publish", func(t *testing.T) {
		detail, command := newAudit(t, false, true)
		id := detail.Summary.TransportID
		proxy := startQuarantineSQLProxy(t, f.dsn.Addr)
		address := proxy.address
		proxy.Close()
		result, err := replay(t, id, "", replayCLIConfig(t, f, address, ""))
		if err == nil || result.CommandID != "" {
			t.Fatal("SQL-unavailable preflight started publication")
		}
		read(t, id, "open", "")
		f.checkEmpty(t)
		if _, err := replay(t, id, "", file); err != nil {
			t.Fatal(err)
		}
		consume(t, command, 1, database.CommandPersisted)
		finish(t, id, database.CommandPersisted)
		check(t)
	})

	if len(known) != 12 || acks != 16 || len(seenAttempts) != 16 {
		t.Fatal("unexpected complete physical/audit set", len(known), acks, len(seenAttempts))
	}
	for table, want := range map[string]int{"message_quarantine": 12, "message_command": 12, "message": 11, "message_outbox": 23, "conversation": 1, "message_delivery_cursor": 0} {
		var count int
		if err := f.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&count); err != nil || count != want {
			t.Fatal("final business/audit set mismatch", table, count, want, err)
		}
	}
	if err := migrations.ApplyReliable(ctx, f.pool); err != nil {
		t.Fatal("schema fingerprint changed", err)
	}
	if err := rabbitmq.VerifyLocal(ctx, f.management, f.broker.VHost); err != nil {
		t.Fatal("canonical topology changed", err)
	}
}
