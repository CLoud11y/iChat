package rabbitmq

import (
	"context"
	"math"

	amqp "github.com/rabbitmq/amqp091-go"
)

func publishContext(ctx context.Context) error {
	if ctx == nil {
		return ErrPublisherDeadline
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		return ErrPublisherDeadline
	}
	return nil
}

func (p *Publisher) publish(ctx context.Context, r PublishResult, prepare func() (amqp.Publishing, error)) (PublishResult, error) {
	if err := publishContext(ctx); err != nil {
		return publishFailure(r, err)
	}
	if p == nil {
		return publishFailure(r, ErrPublisherUnavailable)
	}
	s, err := p.acquire()
	if err != nil {
		return publishFailure(r, err)
	}
	// The acquired slot remains exclusive until success or retirement.
	message, err := prepare()
	if err != nil {
		p.release(s)
		return publishFailure(r, err)
	}
	r.PublishAttemptID, r.ConnectionEpoch, r.ChannelEpoch = message.CorrelationId, p.connectionEpoch, s.epoch
	if err := ctx.Err(); err != nil {
		p.release(s)
		return publishFailure(r, err)
	}
	if !s.clean() || !p.active(s) || s.nextTag == 0 || s.nextTag == math.MaxUint64 || s.channel.GetNextPublishSeqNo() != s.nextTag {
		p.retire(s)
		return publishFailure(r, ErrPublisherNotifications)
	}
	r.DeliveryTag = s.nextTag
	r.Outcome = PublishUnknown // Set before handing control to a network write.
	if p.interrupt != nil {
		stop := watchCancellation(ctx, p.interrupt)
		defer stop()
	}
	if err := s.channel.PublishWithContext(ctx, p.exchange, p.routingKey, true, false, message); err != nil {
		p.retire(s)
		return publishFailure(r, ErrPublisherWrite)
	}
	for {
		select {
		case <-ctx.Done():
			p.retire(s)
			return publishFailure(r, ctx.Err())
		case <-p.done:
			p.retire(s)
			return publishFailure(r, ErrPublisherUnavailable)
		case <-s.closes:
			p.retire(s)
			return publishFailure(r, ErrPublisherUnavailable)
		case returned, ok := <-s.returns:
			return p.returned(s, r, message, returned, ok)
		case confirm, ok := <-s.confirms:
			if !ok || confirm.DeliveryTag != r.DeliveryTag {
				p.retire(s)
				return publishFailure(r, ErrPublisherNotifications)
			}
			return p.confirmed(ctx, s, r, message, confirm)
		}
	}
}
