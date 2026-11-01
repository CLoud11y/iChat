package clientsim

import (
	"context"

	"iChat/messaging"
)

// ObserveCommandResult accepts bytes from an authenticated current WebSocket.
// The caller owns connection/session selection, ready/ping and the read limit.
// true means a valid reference needs a later status query; it never invents a
// full message or starts a concurrent HTTP request while Submit is in flight.
func (r *HTTPRunner) ObserveCommandResult(ctx context.Context, body []byte) (bool, error) {
	if ctx == nil {
		return false, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false, ErrHTTPClosed
	}
	r.active.Add(1)
	r.mu.Unlock()
	defer r.active.Done()
	work, cancel := context.WithTimeout(ctx, r.options.StoreTimeout)
	defer cancel()
	stop := context.AfterFunc(r.work, cancel)
	defer stop()
	// Notifications have their own single slot, so a pending HTTP receipt cannot
	// prevent an earlier terminal result from being saved. No unbounded queue.
	select {
	case r.notices <- struct{}{}:
	case <-work.Done():
		return false, work.Err()
	}
	defer func() { <-r.notices }()
	value, err := messaging.DecodeCommandResult(body, r.sender.user)
	if err != nil {
		return false, ErrProtocol
	}
	e, err := r.entry(work, value.ClientMessageID)
	if err != nil {
		return false, err
	}
	command, err := messaging.Normalize(r.sender.user, e.Request)
	if err != nil || value.PayloadHash != e.PayloadHash {
		return false, ErrProtocol
	}
	if value.State == "persisted" {
		if value.MessageID != e.CommandID || value.ConversationID != command.ConversationID {
			return false, ErrProtocol
		}
		if value.Message == nil {
			if e.State == Persisted {
				if e.Result == nil || e.Result.Message == nil || value.Seq == nil || e.Result.Message.Seq != *value.Seq {
					return false, ErrContradiction
				}
				return false, nil
			}
			if e.State == Rejected {
				return false, ErrContradiction
			}
			return true, nil
		}
	}
	status := messaging.CommandStatus{ClientMessageID: value.ClientMessageID, PayloadHash: value.PayloadHash, State: messaging.StatusState(value.State), ReasonCode: value.ReasonCode, Message: value.Message}
	return false, r.sender.ApplyStatus(work, status)
}
