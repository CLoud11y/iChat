//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/tests/clientsim"
)

func TestProductionSenderConfirmLossRecoveryIntegration(t *testing.T) {
	for _, committed := range []bool{false, true} {
		mode := "pending_query_authorizes_same_key_retry"
		if committed {
			mode = "committed_query_prevents_retry"
		}
		t.Run(mode, func(t *testing.T) {
			var amqpProxy *senderConfirmProxy
			f := newSenderResultFixtureWithOptions(t, nil, func(original string) string {
				u, err := url.Parse(original)
				if err != nil || u.Scheme != "amqp" || u.Host == "" {
					t.Fatal("invalid owned confirm fixture URI")
				}
				amqpProxy = newSenderConfirmProxy(t, u.Host)
				u.Host = amqpProxy.listener.Addr().String()
				return u.String()
			})
			ready, cancelReady := context.WithTimeout(f.ctx, 3*time.Second)
			firstEpoch, err := f.publisher.WaitReady(ready)
			cancelReady()
			if err != nil || firstEpoch == 0 {
				t.Fatal("publisher not initially ready", err)
			}
			var pipeline *senderPipeline
			if committed {
				pipeline = f.startSenderPipeline(t)
			}
			amqpProxy.armed.Store(true)
			httpProxy := newSenderRecoveryProxyWithOptions(t, "http://"+apiAddress(f.api), senderRecoveryProxyOptions{
				allowPublishUnknown: true, holdSecondPost: !committed,
			})
			defer httpProxy.open()
			options, directory := senderGuardOptions(f, httpProxy), t.TempDir()
			local := f.openPipelineSenderWithOptions(t, directory, options)
			request := senderGuardRequest(f)
			entry, err := local.sender.Queue(f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			command, err := messaging.Normalize(uint64(f.users[0].ID), request)
			if err != nil {
				t.Fatal(err)
			}
			if err := local.runner.Submit(f.ctx, entry.ClientMessageID); !errors.Is(err, clientsim.ErrHTTPUnavailable) {
				t.Fatal("lost real AMQP confirm did not return unavailable", err)
			}
			select {
			case ack := <-amqpProxy.dropped:
				if ack.connection == 0 || ack.channel == 0 || ack.tag == 0 {
					t.Fatal("invalid actually observed broker ACK")
				}
				t.Logf("dropped real Basic.Ack: connection=%d channel=%d deliveryTag=%d multiple=%v", ack.connection, ack.channel, ack.tag, ack.multiple)
			default:
				t.Fatal("no real broker confirm was dropped")
			}
			before := local.entry(t, f.ctx)
			if before.State != clientsim.PublishUnknown || before.Attempts != 1 || before.InFlight || before.Result != nil || before.Request != request {
				t.Fatal("actual confirm loss was not durably unknown")
			}
			trace := httpProxy.snapshot()
			if len(trace) != 1 || trace[0].method != http.MethodPost || trace[0].status != 503 {
				t.Fatal("HTTP response was lost/faked instead of actual confirm loss")
			}
			unknown, err := messaging.DecodeSubmissionResponse(trace[0].response)
			if err != nil || unknown.Code != -1 || unknown.Data.State != "publish_unknown" || unknown.Data.ReasonCode != "PUBLISH_UNKNOWN" ||
				!unknown.Data.Matches(command) || !messaging.ValidPublishAttemptID(unknown.Data.PublishAttemptID) {
				t.Fatal("unknown receipt lost canonical attempt/identity", err)
			}
			ready, cancelReady = context.WithTimeout(f.ctx, 20*time.Second)
			newEpoch, err := f.publisher.WaitReady(ready)
			cancelReady()
			if err != nil || newEpoch <= firstEpoch || amqpProxy.accepted.Load() < 2 || amqpProxy.pubs.Load() != 1 || amqpProxy.acks.Load() != 1 || amqpProxy.invalid.Load() {
				t.Fatal("new publisher epoch replayed or failed to recover original publication", err)
			}
			if !committed {
				queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
				if err != nil || queue.Messages != 1 || queue.Consumers != 0 {
					t.Fatal("real confirmed command not retained after lost ACK", err)
				}
				for _, table := range []string{"message_command", "message", "message_outbox"} {
					var n int
					if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != 0 {
						t.Fatal("unconsumed confirm-loss command already persisted", table, n, err)
					}
				}
			}
			local.close(t)
			local = f.openPipelineSenderWithOptions(t, directory, options)
			if !reflect.DeepEqual(before, local.entry(t, f.ctx)) {
				t.Fatal("confirm-loss unknown checkpoint changed on restart")
			}
			policy := clientsim.DefaultRecoveryOptions()
			policy.MaxRounds, policy.MaxSubmissions = 3, 1
			var status messaging.CommandStatus
			if committed {
				status = awaitSenderGuardTerminal(t, f, entry)
				if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil {
					t.Fatal("committed command did not recover by query", err)
				}
			} else {
				done := make(chan error, 1)
				go func() { done <- local.runner.Recover(f.ctx, entry.ClientMessageID, policy) }()
				select {
				case <-httpProxy.held:
				case err := <-done:
					t.Fatal("recovery ended before real same-key retry acceptance", err)
				case <-time.After(8 * time.Second):
					t.Fatal("pending query did not permit bounded retry")
				}
				trace := httpProxy.snapshot()
				if len(trace) != 3 || trace[1].method != http.MethodGet || trace[2].method != http.MethodPost || trace[2].status != 202 || !bytes.Equal(trace[0].request, trace[2].request) {
					t.Fatal("confirm-loss retry changed original request or skipped query")
				}
				pending, err := messaging.DecodeCommandStatusResponse(trace[1].response)
				query, queryErr := messaging.NormalizeStatusQuery(uint64(f.users[0].ID), entry.ClientMessageID, entry.PayloadHash)
				if err != nil || queryErr != nil || trace[1].status != 200 || pending.Code != 0 || pending.Data.State != messaging.StatusPendingOrUnknown ||
					pending.Data.ValidateFor(query) != nil || trace[2].at.Sub(trace[1].at) < time.Duration(pending.Data.PollAfterMs)*time.Millisecond {
					t.Fatal("retry lacked real valid pending evidence/server delay", err)
				}
				accepted, err := messaging.DecodeSubmissionResponse(trace[2].response)
				if err != nil || accepted.Code != 0 || !accepted.Data.Matches(command) || !messaging.ValidPublishAttemptID(accepted.Data.PublishAttemptID) ||
					accepted.Data.PublishAttemptID == unknown.Data.PublishAttemptID {
					t.Fatal("retry reused attempt ID or changed logical identity", err)
				}
				queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
				if err != nil || queue.Messages != 2 || queue.Consumers != 0 || amqpProxy.pubs.Load() != 2 || amqpProxy.acks.Load() != 2 {
					t.Fatal("two actual publications not preserved before production consumption", err)
				}
				pipeline = f.startSenderPipeline(t)
				status = awaitSenderGuardTerminal(t, f, entry)
				httpProxy.open()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal("same-key confirm-loss recovery did not finish", err)
					}
				case <-time.After(8 * time.Second):
					t.Fatal("confirm-loss recovery did not join")
				}
			}
			expected, exchanges := uint64(2), 4
			if committed {
				expected, exchanges = 1, 2
			}
			terminal := local.entry(t, f.ctx)
			if terminal.State != clientsim.Persisted || uint64(terminal.Attempts) != expected || terminal.InFlight || terminal.Request != request ||
				terminal.CommandID != entry.CommandID || terminal.PayloadHash != entry.PayloadHash || !reflect.DeepEqual(terminal.Result, &status) {
				t.Fatal("confirm-loss terminal differs from original logical command")
			}
			body := f.readSenderResult(t, entry.ClientMessageID)
			if query, err := local.runner.ObserveCommandResult(f.ctx, body); err != nil || query || !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
				t.Fatal("late real result changed queried terminal", err)
			}
			local.close(t)
			local = f.openPipelineSenderWithOptions(t, directory, options)
			if !reflect.DeepEqual(terminal, local.entry(t, f.ctx)) {
				t.Fatal("confirm-loss terminal changed on disk restart")
			}
			trace = httpProxy.snapshot()
			if len(trace) != exchanges || trace[len(trace)-1].method != http.MethodGet || trace[len(trace)-1].status != 200 ||
				amqpProxy.pubs.Load() != expected || amqpProxy.acks.Load() != expected || !amqpProxy.didDrop.Load() || amqpProxy.invalid.Load() {
				t.Fatal("unexpected hidden publish, confirm count or recovery HTTP")
			}
			select {
			case <-amqpProxy.dropped:
				t.Fatal("proxy dropped more than one real ACK")
			default:
			}
			finishSenderGuard(t, f, pipeline, status)
			t.Logf("epoch %d -> %d; %d actual Basic.Publish / %d actual Basic.Ack (exactly 1 dropped); %d HTTP exchanges; 1 immutable SQL/disk terminal", firstEpoch, newEpoch, expected, expected, exchanges)
		})
	}
}
