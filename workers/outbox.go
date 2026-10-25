package workers

import (
	"context"
	"errors"
	"time"

	"iChat/database"
	"iChat/messaging"
)

var ErrOutboxWorkerInput = errors.New("invalid outbox worker dependencies or bounds")

// Implementations must honor context cancellation and finish their SQL
// transaction before returning. The loop borrows both dependencies.
type OutboxStore interface {
	Claim(context.Context, database.OutboxClaimOptions) ([]database.OutboxLease, error)
	Complete(context.Context, database.OutboxLease) (database.OutboxSettlement, error)
	Retry(context.Context, database.OutboxLease, database.OutboxRetryOptions, database.OutboxRetryReason) (database.OutboxSettlement, error)
}
type NotificationPublisher interface {
	Publish(context.Context, messaging.Notification) error
}

type OutboxOptions struct {
	Claim                                                         database.OutboxClaimOptions
	Retry                                                         database.OutboxRetryOptions
	ClaimTimeout, PublishTimeout, SettlementTimeout, PollInterval time.Duration
}

func (o OutboxOptions) Validate() error {
	if o.Claim.Validate() != nil || o.Retry.Validate() != nil || o.ClaimTimeout <= 0 || o.ClaimTimeout > time.Minute ||
		o.PublishTimeout <= 0 || o.SettlementTimeout <= 0 || o.PublishTimeout >= o.Claim.Lease ||
		o.SettlementTimeout >= o.Claim.Lease-o.PublishTimeout || o.PollInterval < time.Millisecond || o.PollInterval > time.Minute {
		return ErrOutboxWorkerInput
	}
	return nil
}

type OutboxLoop struct {
	store     OutboxStore
	publisher NotificationPublisher
	options   OutboxOptions
}

func NewOutboxLoop(store OutboxStore, publisher NotificationPublisher, options OutboxOptions) (*OutboxLoop, error) {
	if store == nil || publisher == nil || options.Validate() != nil {
		return nil, ErrOutboxWorkerInput
	}
	return &OutboxLoop{store: store, publisher: publisher, options: options}, nil
}

type OutboxBatch struct{ Claimed, Published, Completed, Retried, Deferred int }

type OutboxError struct {
	Stage string
	cause error
}

func (e *OutboxError) Error() string    { return "outbox execution: " + e.Stage }
func (e *OutboxError) GoString() string { return e.Error() }
func (e *OutboxError) Unwrap() error    { return e.cause }

// RunBatch is sequential and bounded by claim count and per-operation deadlines.
// Its counters preserve earlier confirmed progress on error; Published only
// means Redis returned success, never client delivery. Unknown settlement is
// not repeated or compensated. Remaining leases recover by expiry.
func (w *OutboxLoop) RunBatch(ctx context.Context) (result OutboxBatch, err error) {
	if w == nil || w.store == nil || w.publisher == nil || ctx == nil {
		return result, ErrOutboxWorkerInput
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	start := time.Now()
	claimCtx, cancel := context.WithTimeout(ctx, w.options.ClaimTimeout)
	leases, err := w.store.Claim(claimCtx, w.options.Claim)
	cancel()
	if err != nil {
		return result, &OutboxError{Stage: "claim", cause: err}
	}
	if len(leases) > w.options.Claim.Limit {
		return result, &OutboxError{Stage: "claim invariant", cause: database.ErrOutboxInvariant}
	}
	result.Claimed, result.Deferred = len(leases), len(leases)
	// Claim samples DB time after this monotonic start. This conservative
	// local budget includes SQL/commit latency and previous batch items, and
	// never compares the database wall clock with the host wall clock.
	deadline := start.Add(w.options.Claim.Lease)
	for _, lease := range leases {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if time.Until(deadline) <= w.options.SettlementTimeout {
			break
		}
		n, decodeErr := messaging.DecodeOutboxNotification(messaging.OutboxRecord{EventID: lease.EventID, CommandID: lease.CommandID, EventType: lease.EventType, ConversationID: lease.ConversationID, MessageID: lease.MessageID, Payload: lease.Payload})
		reason := database.OutboxRetryInvalidPayload
		published := false
		if decodeErr == nil {
			publishCtx, stop := context.WithDeadline(ctx, deadline.Add(-w.options.SettlementTimeout))
			publishCtx, timeout := context.WithTimeout(publishCtx, w.options.PublishTimeout)
			publishErr := w.publisher.Publish(publishCtx, n)
			published = publishErr == nil && publishCtx.Err() == nil
			timeout()
			stop()
			// Any attempted Redis call with no timely success may already have
			// published. Retrying is explicitly at least once.
			reason = database.OutboxRetryPublishUnknown
			if published {
				result.Published++
			}
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if time.Until(deadline) <= 0 {
			break
		}
		settleCtx, stop := context.WithDeadline(ctx, deadline)
		settleCtx, timeout := context.WithTimeout(settleCtx, w.options.SettlementTimeout)
		var receipt database.OutboxSettlement
		if published {
			receipt, err = w.store.Complete(settleCtx, lease)
		} else {
			receipt, err = w.store.Retry(settleCtx, lease, w.options.Retry, reason)
		}
		timeout()
		stop()
		if err != nil {
			return result, &OutboxError{Stage: "settlement", cause: err}
		}
		if receipt.EventID != lease.EventID || (published && (receipt.Status != "completed" || receipt.CompletedAt == nil || receipt.AvailableAt != nil)) ||
			(!published && (receipt.Status != "pending" || receipt.AvailableAt == nil || receipt.CompletedAt != nil)) {
			return result, &OutboxError{Stage: "settlement invariant", cause: database.ErrOutboxInvariant}
		}
		result.Deferred--
		if published {
			result.Completed++
		} else {
			result.Retried++
		}
	}
	return result, nil
}

// Run polls even after empty/partial batches: SKIP LOCKED is not evidence that
// the queue is empty. Errors return to the lifecycle owner without a hot retry.
func (w *OutboxLoop) Run(ctx context.Context) error {
	for {
		if _, err := w.RunBatch(ctx); err != nil {
			return err
		}
		timer := time.NewTimer(w.options.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
