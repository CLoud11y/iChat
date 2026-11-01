//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/tests/clientsim"
)

type senderRecoveryExchange struct {
	method, path string
	query        url.Values
	request      []byte
	response     []byte
	status       int
	at           time.Time
}

type senderRecoveryAction uint8

const (
	senderRecoveryForward senderRecoveryAction = iota
	senderRecoveryDrop
	senderRecoveryHold
)

type senderRecoveryProxyOptions struct {
	dropFirstPost, holdSecondPost bool
	statusAction                  func(senderRecoveryExchange) senderRecoveryAction
}

// Forwards actual production responses without changing their contents. Faults
// only drop or delay already-read responses. It never publishes or touches SQL.
type senderRecoveryProxy struct {
	server        *httptest.Server
	mu            sync.Mutex
	exchanges     []senderRecoveryExchange
	posts         int
	held, release chan struct{}
	releaseOnce   sync.Once
	heldOnce      sync.Once
}

func newSenderRecoveryProxy(t *testing.T, origin string) *senderRecoveryProxy {
	t.Helper()
	return newSenderRecoveryProxyWithOptions(t, origin, senderRecoveryProxyOptions{dropFirstPost: true, holdSecondPost: true})
}

func newSenderRecoveryProxyWithOptions(t *testing.T, origin string, options senderRecoveryProxyOptions) *senderRecoveryProxy {
	t.Helper()
	p := &senderRecoveryProxy{held: make(chan struct{}), release: make(chan struct{})}
	transport := &http.Transport{Proxy: nil, DisableCompression: true,
		DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, ResponseHeaderTimeout: 3 * time.Second}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func() {
			t.Error("production recovery proxy could not validate/forward exchange")
			w.WriteHeader(http.StatusBadGateway)
		}
		if !((r.Method == http.MethodPost && r.URL.Path == "/auth/sendMessage") ||
			(r.Method == http.MethodGet && r.URL.Path == "/auth/messages/status")) {
			fail()
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, messaging.MaxBodyBytes+1))
		if err != nil || len(body) > messaging.MaxBodyBytes {
			fail()
			return
		}
		upstream, err := http.NewRequestWithContext(r.Context(), r.Method, origin+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			fail()
			return
		}
		upstream.GetBody = nil // The proxy must not replay the POST either.
		upstream.Header = r.Header.Clone()
		response, err := client.Do(upstream)
		if err != nil {
			fail()
			return
		}
		defer response.Body.Close()
		result, err := io.ReadAll(io.LimitReader(response.Body, messaging.MaxBodyBytes+1))
		if err != nil || len(result) > messaging.MaxBodyBytes {
			fail()
			return
		}
		if r.Method == http.MethodPost {
			value, err := messaging.DecodeSubmissionResponse(result)
			if err != nil || response.StatusCode != 202 || value.Code != 0 || value.Data.State != "accepted" ||
				!messaging.ValidPublishAttemptID(value.Data.PublishAttemptID) {
				fail()
				return
			}
		}
		exchange := senderRecoveryExchange{method: r.Method, path: r.URL.Path,
			query: r.URL.Query(), request: body, response: result, status: response.StatusCode, at: time.Now()}
		p.mu.Lock()
		p.exchanges = append(p.exchanges, exchange)
		if r.Method == http.MethodPost {
			p.posts++
		}
		post := p.posts
		p.mu.Unlock()
		action := senderRecoveryForward
		if r.Method == http.MethodGet && options.statusAction != nil {
			action = options.statusAction(exchange)
		} else if r.Method == http.MethodPost {
			if post > 2 {
				fail()
				return
			}
			if post == 1 && options.dropFirstPost {
				action = senderRecoveryDrop
			} else if post == 2 && options.holdSecondPost {
				action = senderRecoveryHold
			}
		}
		switch action {
		case senderRecoveryDrop:
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				fail()
				return
			}
			connection.Close() // No response headers/body reach HTTPRunner.
			return
		case senderRecoveryHold:
			p.heldOnce.Do(func() { close(p.held) })
			select {
			case <-p.release:
			case <-r.Context().Done():
				return
			}
		}
		for key, values := range response.Header {
			w.Header()[key] = append([]string(nil), values...)
		}
		w.WriteHeader(response.StatusCode)
		if _, err := w.Write(result); err != nil {
			t.Error("production recovery proxy response write failed")
		}
	}))
	t.Cleanup(func() {
		p.open()
		p.server.Close()
		transport.CloseIdleConnections()
	})
	return p
}

func (p *senderRecoveryProxy) open() { p.releaseOnce.Do(func() { close(p.release) }) }
func (p *senderRecoveryProxy) snapshot() []senderRecoveryExchange {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]senderRecoveryExchange(nil), p.exchanges...)
}

func TestProductionSenderLostResponseRecoveryIntegration(t *testing.T) {
	for _, committed := range []bool{false, true} {
		mode := "pending_query_authorizes_original_resend"
		if committed {
			mode = "persisted_query_prevents_resend"
		}
		t.Run(mode, func(t *testing.T) {
			f := newSenderResultFixture(t)
			if err := f.conn.Close(); err != nil {
				t.Fatal(err)
			}
			personal := "ichat:notify:v1:user:" + strconv.FormatUint(uint64(f.users[0].ID), 10)
			apiRealEventually(t, func() bool {
				counts, err := f.client.PubSubNumSub(f.ctx, personal).Result()
				return err == nil && counts[personal] == 0
			})
			proxy := newSenderRecoveryProxy(t, "http://"+apiAddress(f.api))
			defer proxy.open()
			options := clientsim.DefaultHTTPOptions(proxy.server.URL, func(context.Context) (string, error) { return f.token, nil })
			options.MaxConcurrent, options.RequestTimeout = 1, 10*time.Second
			directory := t.TempDir()
			local := f.openPipelineSenderWithOptions(t, directory, options)
			request := messaging.SendRequest{ClientMessageID: "ABCD9012-1234-4234-8234-000000000001", TargetType: messaging.Private,
				TargetID: "000" + strconv.FormatUint(uint64(f.users[1].ID), 10), Media: messaging.Text,
				Content: "lost-production-response \r\n中🙂\x00", ClientSentAt: "2026-11-02T12:34:56.123456789+11:00"}
			entry, err := local.sender.Queue(f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			command, err := messaging.Normalize(uint64(f.users[0].ID), request)
			if err != nil {
				t.Fatal(err)
			}
			repo, err := database.NewMessageRepository(f.pool)
			if err != nil {
				t.Fatal(err)
			}
			var pipeline *senderPipeline
			if committed {
				pipeline = f.startSenderPipeline(t)
			}
			if err := local.runner.Submit(f.ctx, entry.ClientMessageID); !errors.Is(err, clientsim.ErrHTTPUnavailable) {
				t.Fatal("lost accepted response did not report unavailable", err)
			}
			if got := local.entry(t, f.ctx); got.State != clientsim.PublishUnknown || got.Attempts != 1 || got.InFlight || got.Result != nil {
				t.Fatal("lost actual 202 did not leave durable unknown")
			}
			first := proxy.snapshot()
			if len(first) != 1 || first[0].method != http.MethodPost {
				t.Fatal("HTTP automatically replayed the lost POST")
			}
			var status messaging.CommandStatus
			awaitPersisted := func() {
				t.Helper()
				apiRealEventually(t, func() bool {
					status, err = repo.GetCommandStatus(f.ctx, uint64(f.users[0].ID), entry.ClientMessageID, entry.PayloadHash)
					return err == nil && status.State == messaging.StatusPersisted
				})
				apiRealEventually(t, func() bool {
					var completed int
					err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='completed'").Scan(&completed)
					return err == nil && completed == 2
				})
			}
			if committed {
				awaitPersisted()
			} else {
				queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
				if err != nil || queue.Messages != 1 || queue.Consumers != 0 {
					t.Fatal("first confirmed attempt absent from idle queue", err)
				}
			}
			local.close(t)
			local = f.openPipelineSenderWithOptions(t, directory, options)
			if got := local.entry(t, f.ctx); got.State != clientsim.PublishUnknown || got.Attempts != 1 || got.Request != request || got.InFlight {
				t.Fatal("disk restart changed unknown/original request")
			}
			policy := clientsim.DefaultRecoveryOptions()
			policy.MaxRounds, policy.MaxSubmissions = 3, 1
			if committed {
				// Keep submission budget available: only terminal evidence should
				// prevent a new POST, rather than an artificially zero budget.
				if err := local.runner.Recover(f.ctx, entry.ClientMessageID, policy); err != nil {
					t.Fatal("persisted query did not recover lost response", err)
				}
			} else {
				done := make(chan error, 1)
				go func() { done <- local.runner.Recover(f.ctx, entry.ClientMessageID, policy) }()
				select {
				case <-proxy.held:
				case err := <-done:
					t.Fatal("recovery ended before original-request repost", err)
				case <-time.After(8 * time.Second):
					t.Fatal("pending query did not authorize bounded repost")
				}
				trace := proxy.snapshot()
				if len(trace) != 3 || trace[1].method != http.MethodGet || trace[2].method != http.MethodPost ||
					!bytes.Equal(trace[0].request, trace[2].request) {
					t.Fatal("repost changed request or lacked preceding query")
				}
				pending, err := messaging.DecodeCommandStatusResponse(trace[1].response)
				if err != nil || trace[1].status != 200 || pending.Code != 0 || pending.Data.State != messaging.StatusPendingOrUnknown ||
					pending.Data.PollAfterMs <= 0 ||
					trace[2].at.Sub(trace[1].at) < time.Duration(pending.Data.PollAfterMs)*time.Millisecond {
					t.Fatal("repost lacked valid pending evidence or server wait floor", err)
				}
				queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
				if err != nil || queue.Messages != 2 || queue.Consumers != 0 {
					t.Fatal("two confirmed attempts not preserved before consumption", err)
				}
				pipeline = f.startSenderPipeline(t)
				awaitPersisted()
				proxy.open()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal("same-key recovery did not finish", err)
					}
				case <-time.After(8 * time.Second):
					t.Fatal("terminal recovery query did not join")
				}
			}
			attempts, exchanges := messaging.Sequence(2), 4
			if committed {
				attempts, exchanges = 1, 2
			}
			local.close(t)
			local = f.openPipelineSenderWithOptions(t, directory, options)
			got := local.entry(t, f.ctx)
			if got.State != clientsim.Persisted || got.Attempts != attempts || got.Request != request || got.InFlight ||
				got.ClientMessageID != entry.ClientMessageID || got.CommandID != entry.CommandID || got.PayloadHash != entry.PayloadHash ||
				!reflect.DeepEqual(got.Result, &status) {
				t.Fatal("final disk/SQL/original request differs")
			}
			trace := proxy.snapshot()
			if len(trace) != exchanges {
				t.Fatal("unexpected HTTP recovery request count", len(trace))
			}
			physical := make(map[string]bool)
			for _, exchange := range trace {
				if exchange.method == http.MethodPost {
					value, err := messaging.DecodeSubmissionResponse(exchange.response)
					var sent messaging.SendRequest
					if err != nil || !value.Data.Matches(command) || json.Unmarshal(exchange.request, &sent) != nil || sent != request ||
						physical[value.Data.PublishAttemptID] {
						t.Fatal("physical attempts changed logical identity/request or reused attempt ID", err)
					}
					physical[value.Data.PublishAttemptID] = true
				} else if exchange.query.Get("clientMessageId") != entry.ClientMessageID || exchange.query.Get("payloadHash") != entry.PayloadHash {
					t.Fatal("recovery query changed stable identity")
				}
			}
			if len(physical) != int(attempts) || trace[len(trace)-1].method != http.MethodGet {
				t.Fatal("unexpected publication count or final recovery action")
			}
			c, cancel := context.WithTimeout(f.ctx, 5*time.Second)
			defer cancel()
			if err := pipeline.persist.Drain(c); err != nil {
				t.Fatal(err)
			}
			if err := pipeline.persist.Close(c); err != nil {
				t.Fatal(err)
			}
			if err := pipeline.session.WaitClosed(c); err != nil {
				t.Fatal(err)
			}
			if err := pipeline.drainOutbox(c); err != nil {
				t.Fatal(err)
			}
			apiRealEventually(t, func() bool {
				queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
				return err == nil && queue.Consumers == 0
			})
			queue, err := f.channel.QueueInspect(rabbitmq.PersistQueue)
			if err != nil || queue.Messages != 0 || queue.Consumers != 0 {
				t.Fatal("production ACK did not settle every physical attempt", err)
			}
			for table, want := range map[string]int{"message_command": 1, "message": 1, "conversation": 1, "message_outbox": 2, "message_delivery_cursor": 0, "message_quarantine": 0} {
				var n int
				if err := f.pool.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != want {
					t.Fatal("duplicate changed final logical collection", table, n, err)
				}
			}
			var watermark uint64
			if err := f.pool.QueryRowContext(f.ctx, "SELECT last_seq FROM conversation").Scan(&watermark); err != nil || watermark != 1 {
				t.Fatal("duplicate allocated a second sequence", err)
			}
			after, err := repo.GetCommandStatus(f.ctx, uint64(f.users[0].ID), entry.ClientMessageID, entry.PayloadHash)
			if err != nil || !reflect.DeepEqual(after, status) {
				t.Fatal("deduplication changed original SQL terminal", err)
			}
			if err := migrations.ApplyReliable(f.ctx, f.pool); err != nil {
				t.Fatal("schema fingerprint changed", err)
			}
			t.Logf("%d actual confirmed publications / %d HTTP exchanges / 1 durable terminal; final 1 command / 1 message / 1 conversation last_seq=1 / 2 completed Outbox / 0 cursor / 0 quarantine; queue ready=0 consumers=0", attempts, exchanges)
		})
	}
}
