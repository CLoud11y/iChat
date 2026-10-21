package rabbitmq

import (
	"encoding/hex"
	"errors"
	"time"

	"iChat/messaging"

	amqp "github.com/rabbitmq/amqp091-go"
)

var ErrQuarantineInput = errors.New("invalid quarantine audit input")

type QuarantineInputFormat string

const (
	QuarantineWrapped    QuarantineInputFormat = "active_wrapper"
	QuarantineDeadLetter QuarantineInputFormat = "broker_dlx"
)

// QuarantineBusinessIdentity is enrichment from a fully validated source wire,
// never a best-effort extraction of fields from malformed JSON.
type QuarantineBusinessIdentity struct{ SenderID, ClientMessageID, CommandID, PayloadHash string }

// QuarantineInput owns immutable source and delivery snapshots, not an ACK
// capability. Parsing alone never proves that an audit record was committed.
type QuarantineInput struct {
	format     QuarantineInputFormat
	envelope   QuarantineEnvelope
	metadata   quarantineSnapshot // Wrapper delivery metadata without duplicating its body.
	receivedAt time.Time
	identity   QuarantineBusinessIdentity
	identified bool
}

type quarantineInputError struct {
	stage string
	cause error
}

func (e *quarantineInputError) Error() string        { return "quarantine audit input: " + e.stage }
func (e *quarantineInputError) GoString() string     { return e.Error() }
func (e *quarantineInputError) Unwrap() error        { return e.cause }
func (e *quarantineInputError) Is(target error) bool { return target == ErrQuarantineInput }

func quarantineInputFailure(stage string, cause error) (QuarantineInput, error) {
	return QuarantineInput{}, &quarantineInputError{stage: stage, cause: cause}
}

func ParseQuarantineInput(delivery ConsumerMessage, receivedAt time.Time) (QuarantineInput, error) {
	if receivedAt.IsZero() {
		return quarantineInputFailure("observation", nil)
	}
	if _, err := receivedAt.UTC().MarshalJSON(); err != nil {
		return quarantineInputFailure("observation", nil)
	}
	if delivery.Exchange != QuarantineExchange || delivery.RoutingKey != QuarantineRoutingKey {
		return quarantineInputFailure("route", nil)
	}
	if len(delivery.Body) > MaxQuarantineBytes {
		return quarantineInputFailure("body budget", ErrQuarantineEnvelope)
	}
	result := QuarantineInput{receivedAt: receivedAt.UTC()}
	var source ConsumerMessage
	var err error
	if delivery.ContentType == QuarantineContentType || delivery.Type == QuarantineMessageType {
		// A partial/invalid wrapper never falls back to the raw DLX branch.
		if delivery.ContentType != QuarantineContentType || delivery.Type != QuarantineMessageType || delivery.ContentEncoding != "" || delivery.DeliveryMode != amqp.Persistent ||
			delivery.Expiration != "" || delivery.Priority != 0 || delivery.ReplyTo != "" || delivery.UserID != "" || delivery.AppID != "" || !delivery.Timestamp.IsZero() || !quarantineAttempt(delivery.CorrelationID) {
			return quarantineInputFailure("wrapper properties", nil)
		}
		result.envelope, err = DecodeQuarantineEnvelope(delivery.Body)
		if err != nil {
			return quarantineInputFailure("wrapper envelope", err)
		}
		if delivery.MessageID != result.envelope.TransportID() {
			return quarantineInputFailure("wrapper transport identity", nil)
		}
		if code := result.envelope.ErrorCode(); code != "INVALID_DELIVERY" && code != "DATA_CONSTRAINT" {
			return quarantineInputFailure("wrapper error code", nil)
		}
		metadata := delivery
		metadata.Body = nil
		result.metadata, err = snapshotQuarantineMessage(metadata)
		if err != nil {
			return quarantineInputFailure("delivery metadata", err)
		}
		result.format = QuarantineWrapped
		source, err = result.envelope.Source()
	} else {
		// Capture/bound the complete table before interpreting arbitrary nested
		// header values. No input mutation or JSON number coercion is allowed.
		snapshot, captureErr := snapshotQuarantineMessage(delivery)
		if captureErr != nil {
			return quarantineInputFailure("delivery snapshot", captureErr)
		}
		cloned, cloneErr := snapshot.message()
		if cloneErr != nil {
			return quarantineInputFailure("delivery snapshot", cloneErr)
		}
		code := quarantineDeadLetterCode(cloned.Headers)
		if code == "" {
			return quarantineInputFailure("dead letter provenance", nil)
		}
		result.envelope, err = NewQuarantineEnvelope(cloned, code, result.receivedAt)
		result.format = QuarantineDeadLetter
		source = cloned
		// Only the enrichment candidate recovers the original command route;
		// the immutable envelope retains the actual DLX delivery metadata.
		source.Exchange, source.RoutingKey = CommandsExchange, SubmitRoutingKey
	}
	if err != nil {
		return quarantineInputFailure("source snapshot", err)
	}
	result.identity, result.identified = quarantineBusinessIdentity(source)
	return result, nil
}

func (q QuarantineInput) Format() QuarantineInputFormat { return q.format }
func (q QuarantineInput) Envelope() QuarantineEnvelope  { return q.envelope }
func (q QuarantineInput) ReceivedAt() time.Time         { return q.receivedAt }
func (q QuarantineInput) Identity() (QuarantineBusinessIdentity, bool) {
	return q.identity, q.identified
}
func (q QuarantineInput) String() string {
	return "quarantine audit input " + string(q.format) + " " + q.envelope.TransportID()
}
func (q QuarantineInput) GoString() string { return q.String() }

// Delivery returns the current quarantine delivery bytes and metadata as a
// fresh data copy. Source() on Envelope returns the underlying command bytes.
func (q QuarantineInput) Delivery() (ConsumerMessage, error) {
	if q.format == QuarantineDeadLetter {
		return q.envelope.Source()
	}
	if q.format != QuarantineWrapped {
		return ConsumerMessage{}, ErrQuarantineInput
	}
	message, err := q.metadata.message()
	if err != nil {
		return ConsumerMessage{}, err
	}
	message.Body = q.envelope.Encode()
	return message, nil
}

func quarantineAttempt(attempt string) bool {
	if len(attempt) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(attempt)
	return err == nil && hex.EncodeToString(decoded) == attempt
}

func quarantineBusinessIdentity(source ConsumerMessage) (QuarantineBusinessIdentity, bool) {
	if source.ContentType != "application/json" || source.ContentEncoding != "" || source.DeliveryMode != amqp.Persistent || source.Exchange != CommandsExchange || source.RoutingKey != SubmitRoutingKey {
		return QuarantineBusinessIdentity{}, false
	}
	envelope, err := messaging.DecodeEnvelope(source.Body)
	if err != nil || source.MessageID != envelope.CommandID || source.CorrelationID != envelope.PublishAttemptID {
		return QuarantineBusinessIdentity{}, false
	}
	command, err := messaging.ValidateEnvelope(envelope)
	if err != nil {
		return QuarantineBusinessIdentity{}, false
	}
	return QuarantineBusinessIdentity{SenderID: command.SenderID, ClientMessageID: command.ClientMessageID, CommandID: command.CommandID, PayloadHash: command.PayloadHash}, true
}

func quarantineDeadLetterCode(headers amqp.Table) string {
	deaths, ok := headers["x-death"].([]any)
	if !ok || len(deaths) == 0 {
		return ""
	}
	for _, item := range deaths {
		death, ok := item.(amqp.Table)
		if !ok {
			return ""
		}
		queue, qok := death["queue"].(string)
		exchange, eok := death["exchange"].(string)
		reason, rok := death["reason"].(string)
		count, cok := death["count"].(int64)
		stamp, tok := death["time"].(time.Time)
		keys, kok := death["routing-keys"].([]any)
		if !qok || queue == "" || len(queue) > 255 || !eok || len(exchange) > 255 || !rok || deadLetterErrorCode(reason) == "" || !cok || count < 1 || !tok || stamp.IsZero() || !kok || len(keys) == 0 {
			return ""
		}
		for _, key := range keys {
			if value, ok := key.(string); !ok || len(value) > 255 {
				return ""
			}
		}
	}
	latest := deaths[0].(amqp.Table)
	if latest["queue"] != PersistQueue || latest["exchange"] != CommandsExchange {
		return ""
	}
	routed := false
	for _, key := range latest["routing-keys"].([]any) {
		if key == SubmitRoutingKey {
			routed = true
		}
	}
	if !routed {
		return ""
	}
	// Fixed RabbitMQ v1 topology expects the matching last-death triplet.
	if headers["x-last-death-queue"] != PersistQueue || headers["x-last-death-exchange"] != CommandsExchange || headers["x-last-death-reason"] != latest["reason"] {
		return ""
	}
	return deadLetterErrorCode(latest["reason"].(string))
}

func deadLetterErrorCode(reason string) string {
	switch reason {
	case "rejected":
		return "BROKER_REJECTED"
	case "expired":
		return "BROKER_EXPIRED"
	case "maxlen":
		return "BROKER_MAXLEN"
	case "delivery_limit":
		return "BROKER_DELIVERY_LIMIT"
	default:
		return ""
	}
}
