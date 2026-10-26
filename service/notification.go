package service

import (
	"context"
	"errors"
	"reflect"

	"iChat/messaging"
)

var ErrNotificationInput = errors.New("invalid notification dispatcher input")
var ErrNotificationResult = errors.New("notification repository returned an inconsistent frame")

type NotificationReader interface {
	ResolveNotification(context.Context, uint64, messaging.NotificationHint) (messaging.ServerFrame, error)
}
type NotificationDispatcher struct{ reader NotificationReader }

func NewNotificationDispatcher(reader NotificationReader) (*NotificationDispatcher, error) {
	if reader == nil {
		return nil, ErrNotificationInput
	}
	v := reflect.ValueOf(reader)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return nil, ErrNotificationInput
		}
	}
	return &NotificationDispatcher{reader: reader}, nil
}

// Dispatch runs immediately before an enqueue/write attempt. The caller supplies
// the authenticated session UID and a bounded, cancellation-aware enqueue.
// A successful return means only local enqueue, never delivered or persisted ACK.
// P5.3 owns subscriptions, session fencing, bounded queues and the single writer.
func (d *NotificationDispatcher) Dispatch(ctx context.Context, reader uint64, channel string, body []byte, enqueue func(context.Context, messaging.ServerFrame) error) (bool, error) {
	if d == nil || d.reader == nil || ctx == nil || reader == 0 || enqueue == nil {
		return false, ErrNotificationInput
	}
	if _, ok := ctx.Deadline(); !ok {
		return false, ErrNotificationInput
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	h, err := messaging.DecodeNotificationHint(channel, body)
	if err != nil {
		return false, err
	}
	if !h.AllowsUser(reader) {
		return false, nil
	}
	f, err := d.reader.ResolveNotification(ctx, reader, h)
	if err != nil {
		return false, &notificationDispatchError{stage: "resolve", cause: err}
	}
	if !f.Valid() {
		return false, nil
	}
	if !f.Matches(h) {
		return false, ErrNotificationResult
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := enqueue(ctx, f); err != nil {
		return false, &notificationDispatchError{stage: "enqueue", cause: err}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}

type notificationDispatchError struct {
	stage string
	cause error
}

func (e *notificationDispatchError) Error() string    { return "notification dispatch: " + e.stage }
func (e *notificationDispatchError) GoString() string { return e.Error() }
func (e *notificationDispatchError) Unwrap() error    { return e.cause }
