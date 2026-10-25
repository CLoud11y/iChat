package workers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"iChat/database"
	"iChat/messaging"
)

type outboxTestStore struct {
	claim    func(context.Context, database.OutboxClaimOptions) ([]database.OutboxLease, error)
	complete func(context.Context, database.OutboxLease) (database.OutboxSettlement, error)
	retry    func(context.Context, database.OutboxLease, database.OutboxRetryOptions, database.OutboxRetryReason) (database.OutboxSettlement, error)
}

func (s *outboxTestStore) Claim(c context.Context, o database.OutboxClaimOptions) ([]database.OutboxLease, error) {
	return s.claim(c, o)
}
func (s *outboxTestStore) Complete(c context.Context, l database.OutboxLease) (database.OutboxSettlement, error) {
	return s.complete(c, l)
}
func (s *outboxTestStore) Retry(c context.Context, l database.OutboxLease, o database.OutboxRetryOptions, r database.OutboxRetryReason) (database.OutboxSettlement, error) {
	return s.retry(c, l, o, r)
}

type notificationPublisherFunc func(context.Context, messaging.Notification) error

func (f notificationPublisherFunc) Publish(c context.Context, n messaging.Notification) error {
	return f(c, n)
}
func outboxTestOptions() OutboxOptions {
	return OutboxOptions{Claim: database.OutboxClaimOptions{Limit: 3, Lease: time.Second}, Retry: database.OutboxRetryOptions{Base: time.Second, Max: time.Minute}, ClaimTimeout: 100 * time.Millisecond, PublishTimeout: 50 * time.Millisecond, SettlementTimeout: 50 * time.Millisecond, PollInterval: time.Millisecond}
}
func outboxTestLease(t *testing.T) database.OutboxLease {
	t.Helper()
	c, err := messaging.Normalize(2, messaging.SendRequest{ClientMessageID: "12345678-1234-4234-8234-123456789abc", TargetID: "10", TargetType: messaging.Private, Media: messaging.Text, Content: "private-secret"})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := messaging.OutboxEventID(c.CommandID, messaging.EventMessagePersisted)
	body, _ := json.Marshal(messaging.MessagePersistedEvent{SchemaVersion: 1, EventID: id, CommandID: c.CommandID, ConversationID: c.ConversationID, MessageID: c.CommandID, Seq: 1, SenderID: c.SenderID, TargetID: c.TargetID, TargetType: c.TargetType})
	return database.OutboxLease{EventID: id, CommandID: c.CommandID, EventType: messaging.EventMessagePersisted, ConversationID: &c.ConversationID, MessageID: &c.CommandID, Payload: body, Token: "12345678-1234-4234-8234-123456789abc", Until: time.Now().Add(time.Second), Attempts: 1}
}
func outboxTestReceipt(l database.OutboxLease, complete bool) database.OutboxSettlement {
	now := time.Now()
	r := database.OutboxSettlement{EventID: l.EventID, Status: "pending", AvailableAt: &now}
	if complete {
		r.Status, r.CompletedAt, r.AvailableAt = "completed", &now, nil
	}
	return r
}

func TestOutboxLoopSequencingAndDisposition(t *testing.T) {
	for _, mode := range []string{"success", "unknown", "invalid", "claim_unknown", "settle_unknown", "lost", "bad_receipt", "late_success", "cancel_publish"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			l := outboxTestLease(t)
			if mode == "invalid" {
				l.Payload = []byte(`{}`)
			}
			var steps []string
			store := &outboxTestStore{}
			store.claim = func(c context.Context, o database.OutboxClaimOptions) ([]database.OutboxLease, error) {
				steps = append(steps, "claim")
				if _, ok := c.Deadline(); !ok {
					t.Fatal("claim unbounded")
				}
				if mode == "claim_unknown" {
					return nil, database.ErrOutboxCommitUnknown
				}
				return []database.OutboxLease{l}, nil
			}
			store.complete = func(c context.Context, got database.OutboxLease) (database.OutboxSettlement, error) {
				steps = append(steps, "complete")
				if got.Token != l.Token {
					t.Fatal("receipt changed")
				}
				if _, ok := c.Deadline(); !ok {
					t.Fatal("settlement unbounded")
				}
				if mode == "settle_unknown" {
					return database.OutboxSettlement{}, database.ErrOutboxCommitUnknown
				}
				if mode == "lost" {
					return database.OutboxSettlement{}, database.ErrOutboxLeaseLost
				}
				r := outboxTestReceipt(l, true)
				if mode == "bad_receipt" {
					r.EventID = "other"
				}
				return r, nil
			}
			store.retry = func(c context.Context, got database.OutboxLease, o database.OutboxRetryOptions, r database.OutboxRetryReason) (database.OutboxSettlement, error) {
				steps = append(steps, "retry")
				want := database.OutboxRetryPublishUnknown
				if mode == "invalid" {
					want = database.OutboxRetryInvalidPayload
				}
				if r != want || got.Token != l.Token {
					t.Fatal("incorrect retry")
				}
				return outboxTestReceipt(l, false), nil
			}
			pub := notificationPublisherFunc(func(c context.Context, n messaging.Notification) error {
				steps = append(steps, "publish")
				if !n.Valid() {
					t.Fatal("invalid publication")
				}
				if mode == "unknown" {
					return errors.New("private-secret")
				}
				if mode == "cancel_publish" {
					cancel()
					return context.Canceled
				}
				if mode == "late_success" {
					<-c.Done()
					return nil
				}
				return nil
			})
			w, err := NewOutboxLoop(store, pub, outboxTestOptions())
			if err != nil {
				t.Fatal(err)
			}
			r, err := w.RunBatch(ctx)
			switch mode {
			case "claim_unknown":
				if !errors.Is(err, database.ErrOutboxCommitUnknown) || strings.Join(steps, ",") != "claim" || r.Claimed != 0 {
					t.Fatal(r, err, steps)
				}
			case "settle_unknown", "lost", "bad_receipt":
				if err == nil || strings.Join(steps, ",") != "claim,publish,complete" || r.Published != 1 || r.Completed != 0 || r.Deferred != 1 {
					t.Fatal(r, err, steps)
				}
			case "cancel_publish":
				if !errors.Is(err, context.Canceled) || strings.Join(steps, ",") != "claim,publish" || r.Deferred != 1 {
					t.Fatal(r, err, steps)
				}
			case "invalid":
				if err != nil || strings.Join(steps, ",") != "claim,retry" || r.Retried != 1 || r.Published != 0 {
					t.Fatal(r, err, steps)
				}
			case "unknown", "late_success":
				if err != nil || strings.Join(steps, ",") != "claim,publish,retry" || r.Retried != 1 || r.Published != 0 {
					t.Fatal(r, err, steps)
				}
			default:
				if err != nil || strings.Join(steps, ",") != "claim,publish,complete" || r.Completed != 1 || r.Deferred != 0 {
					t.Fatal(r, err, steps)
				}
			}
			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("error leaks body")
			}
		})
	}
}

func TestOutboxLoopBudgetCancellationAndPolling(t *testing.T) {
	l := outboxTestLease(t)
	store := &outboxTestStore{claim: func(ctx context.Context, _ database.OutboxClaimOptions) ([]database.OutboxLease, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	w, _ := NewOutboxLoop(store, notificationPublisherFunc(func(context.Context, messaging.Notification) error { t.Fatal("unexpected publish"); return nil }), outboxTestOptions())
	if _, err := w.RunBatch(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	// Claim latency exhausts the conservative budget; never start a late send.
	o := outboxTestOptions()
	o.ClaimTimeout = 2 * time.Second
	w.options = o
	store.claim = func(context.Context, database.OutboxClaimOptions) ([]database.OutboxLease, error) {
		time.Sleep(time.Second + time.Millisecond)
		return []database.OutboxLease{l}, nil
	}
	r, err := w.RunBatch(context.Background())
	if err != nil || r.Deferred != 1 || r.Published != 0 {
		t.Fatal(r, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	count := 0
	store.claim = func(context.Context, database.OutboxClaimOptions) ([]database.OutboxLease, error) {
		count++
		if count == 3 {
			cancel()
		}
		return nil, nil
	}
	if err := w.Run(ctx); !errors.Is(err, context.Canceled) || count != 3 {
		t.Fatal("empty batch stopped polling", count, err)
	}
	if _, err := w.RunBatch(nil); !errors.Is(err, ErrOutboxWorkerInput) {
		t.Fatal(err)
	}
	if _, err := w.RunBatch(ctx); !errors.Is(err, context.Canceled) || count != 3 {
		t.Fatal("canceled admission")
	}
}

func TestOutboxOptionsAndRedisInput(t *testing.T) {
	for _, mutate := range []func(*OutboxOptions){func(o *OutboxOptions) { o.Claim.Limit = 101 }, func(o *OutboxOptions) { o.Retry.Max = 0 }, func(o *OutboxOptions) { o.ClaimTimeout = 0 }, func(o *OutboxOptions) { o.PublishTimeout = 0 }, func(o *OutboxOptions) { o.SettlementTimeout = 0 }, func(o *OutboxOptions) { o.PublishTimeout = time.Second }, func(o *OutboxOptions) { o.SettlementTimeout = time.Second - o.PublishTimeout }, func(o *OutboxOptions) { o.PollInterval = 0 }} {
		o := outboxTestOptions()
		mutate(&o)
		if o.Validate() == nil {
			t.Fatal("invalid bounds accepted")
		}
	}
	if _, err := NewOutboxLoop(nil, nil, outboxTestOptions()); err == nil {
		t.Fatal("nil dependencies accepted")
	}
	if _, err := NewRedisNotificationPublisher(nil); err == nil {
		t.Fatal("nil redis accepted")
	}
	var p *RedisNotificationPublisher
	if err := p.Publish(context.Background(), messaging.Notification{}); err == nil {
		t.Fatal("nil publisher accepted")
	}
}

func TestOutboxLoopStopsBatchAfterSettlementUncertainty(t *testing.T) {
	l := outboxTestLease(t)
	published, settled := 0, 0
	store := &outboxTestStore{
		claim: func(context.Context, database.OutboxClaimOptions) ([]database.OutboxLease, error) {
			return []database.OutboxLease{l, l, l}, nil
		},
		complete: func(c context.Context, l database.OutboxLease) (database.OutboxSettlement, error) {
			settled++
			if settled == 2 {
				<-c.Done()
				return database.OutboxSettlement{}, c.Err()
			}
			return outboxTestReceipt(l, true), nil
		},
	}
	w, _ := NewOutboxLoop(store, notificationPublisherFunc(func(context.Context, messaging.Notification) error { published++; return nil }), outboxTestOptions())
	r, err := w.RunBatch(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || r.Claimed != 3 || r.Published != 2 || r.Completed != 1 || r.Deferred != 2 || published != 2 || settled != 2 {
		t.Fatal(r, err, published, settled)
	}
}

func TestOutboxRedisPublicationDeadline(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })
	written := make(chan struct{})
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		rd := bufio.NewReader(serverConn)
		line, err := rd.ReadString('\n')
		if err != nil || line != "*3\r\n" {
			return
		}
		for range 3 {
			line, err = rd.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "$") {
				return
			}
			n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
			if err != nil || n < 0 || n > messaging.MaxBodyBytes {
				return
			}
			if _, err := io.CopyN(io.Discard, rd, int64(n+2)); err != nil {
				return
			}
		}
		close(written) // Complete PUBLISH consumed; deliberately omit its reply.
		_, _ = io.Copy(io.Discard, rd)
	}()
	rds := redis.NewClient(&redis.Options{Dialer: func(context.Context, string, string) (net.Conn, error) { return clientConn, nil }, MaxRetries: -1, ReadTimeout: time.Second, WriteTimeout: time.Second, PoolTimeout: time.Second})
	t.Cleanup(func() { rds.Close(); serverConn.Close(); <-peerDone })
	p, _ := NewRedisNotificationPublisher(rds)
	l := outboxTestLease(t)
	n, err := messaging.DecodeOutboxNotification(messaging.OutboxRecord{EventID: l.EventID, CommandID: l.CommandID, EventType: l.EventType, ConversationID: l.ConversationID, MessageID: l.MessageID, Payload: l.Payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(context.Background(), n); !errors.Is(err, ErrOutboxWorkerInput) {
		t.Fatal("unbounded Redis call accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = p.Publish(ctx, n)
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("Redis response wait unbounded", err)
	}
	select {
	case <-written:
	default:
		t.Fatal("publish was not written")
	}
	if strings.Contains(err.Error(), "private-secret") {
		t.Fatal("diagnostic leak")
	}
}
