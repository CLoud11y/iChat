package rabbitmq

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const MaxQuarantineBytes = 4 * 1024 * 1024

var ErrQuarantineEnvelope = errors.New("invalid or oversized quarantine envelope")

// QuarantineEnvelope is an immutable audit snapshot, never an ACK capability.
// Its JSON is a separate protocol from the command publisher's v1 envelope.
type QuarantineEnvelope struct {
	encoded    []byte
	id         string
	code       string
	observedAt time.Time
}

type quarantineWire struct {
	Version     int                `json:"version"`
	TransportID string             `json:"transport_id"`
	ErrorCode   string             `json:"error_code"`
	ObservedAt  time.Time          `json:"observed_at"`
	Source      quarantineSnapshot `json:"source"`
}

type quarantineSnapshot struct {
	Body    []byte           `json:"body"`
	Headers quarantineHeader `json:"headers"`
	// Fixed v1 positions: content type/encoding, correlation ID, reply-to,
	// expiration, message ID, type, user ID, app ID, consumer tag, exchange, key.
	Text         [12][]byte `json:"text"`
	Timestamp    []byte     `json:"timestamp"`
	DeliveryMode uint8      `json:"delivery_mode"`
	Priority     uint8      `json:"priority"`
	DeliveryTag  uint64     `json:"delivery_tag"`
	Redelivered  bool       `json:"redelivered"`
}

// NewQuarantineEnvelope does no I/O and does not validate the command body.
// Unsupported metadata or an exceeded budget returns no partial snapshot.
func NewQuarantineEnvelope(source ConsumerMessage, errorCode string, observedAt time.Time) (QuarantineEnvelope, error) {
	if !quarantineCode(errorCode) || observedAt.IsZero() {
		return QuarantineEnvelope{}, ErrQuarantineEnvelope
	}
	snapshot, err := snapshotQuarantineMessage(source)
	if err != nil {
		return QuarantineEnvelope{}, err
	}
	wire := quarantineWire{Version: 1, ErrorCode: errorCode, ObservedAt: observedAt.UTC(), Source: snapshot}
	wire.TransportID = quarantineIdentity(snapshot)
	encoded, err := json.Marshal(wire)
	if err != nil || len(encoded) > MaxQuarantineBytes {
		return QuarantineEnvelope{}, ErrQuarantineEnvelope
	}
	return QuarantineEnvelope{encoded: encoded, id: wire.TransportID, code: wire.ErrorCode, observedAt: wire.ObservedAt}, nil
}

// DecodeQuarantineEnvelope accepts the canonical representation produced above.
// Re-encoding also rejects duplicate/unknown fields and noncanonical field data.
// Matching the hash is an integrity check, not publisher authentication.
func DecodeQuarantineEnvelope(encoded []byte) (QuarantineEnvelope, error) {
	if len(encoded) == 0 || len(encoded) > MaxQuarantineBytes {
		return QuarantineEnvelope{}, ErrQuarantineEnvelope
	}
	var wire quarantineWire
	if err := json.Unmarshal(encoded, &wire); err != nil || wire.Version != 1 {
		return QuarantineEnvelope{}, ErrQuarantineEnvelope
	}
	source, err := wire.Source.message()
	if err != nil {
		return QuarantineEnvelope{}, err
	}
	rebuilt, err := NewQuarantineEnvelope(source, wire.ErrorCode, wire.ObservedAt)
	if err != nil || !bytes.Equal(rebuilt.encoded, encoded) {
		return QuarantineEnvelope{}, ErrQuarantineEnvelope
	}
	return rebuilt, nil
}

func (q QuarantineEnvelope) TransportID() string   { return q.id }
func (q QuarantineEnvelope) ErrorCode() string     { return q.code }
func (q QuarantineEnvelope) ObservedAt() time.Time { return q.observedAt }
func (q QuarantineEnvelope) Encode() []byte        { return append([]byte(nil), q.encoded...) }
func (q QuarantineEnvelope) String() string        { return fmt.Sprintf("quarantine envelope %s", q.id) }
func (q QuarantineEnvelope) GoString() string      { return q.String() }

// Source returns a fresh copy of all delivered bytes/properties and audit
// metadata. It cannot be used in place of the original ConsumerDelivery handle.
func (q QuarantineEnvelope) Source() (ConsumerMessage, error) {
	var wire quarantineWire
	if len(q.encoded) == 0 || json.Unmarshal(q.encoded, &wire) != nil {
		return ConsumerMessage{}, ErrQuarantineEnvelope
	}
	return wire.Source.message()
}

// RawHeaders is typed JSON suitable for message_quarantine.raw_headers.
func (q QuarantineEnvelope) RawHeaders() ([]byte, error) {
	var wire quarantineWire
	if len(q.encoded) == 0 || json.Unmarshal(q.encoded, &wire) != nil {
		return nil, ErrQuarantineEnvelope
	}
	return json.Marshal(wire.Source.Headers)
}

func (s quarantineSnapshot) message() (ConsumerMessage, error) {
	headers, err := decodeQuarantineHeader(s.Headers, 0, &quarantineBudget{})
	if err != nil {
		return ConsumerMessage{}, err
	}
	table, ok := headers.(amqp.Table)
	if !ok {
		return ConsumerMessage{}, ErrQuarantineEnvelope
	}
	var stamp time.Time
	if err := stamp.UnmarshalBinary(s.Timestamp); err != nil {
		return ConsumerMessage{}, ErrQuarantineEnvelope
	}
	return ConsumerMessage{Body: append([]byte{}, s.Body...), Headers: table,
		ContentType: string(s.Text[0]), ContentEncoding: string(s.Text[1]), CorrelationID: string(s.Text[2]),
		ReplyTo: string(s.Text[3]), Expiration: string(s.Text[4]), MessageID: string(s.Text[5]),
		Type: string(s.Text[6]), UserID: string(s.Text[7]), AppID: string(s.Text[8]),
		ConsumerTag: string(s.Text[9]), Exchange: string(s.Text[10]), RoutingKey: string(s.Text[11]),
		Timestamp: stamp.UTC(), DeliveryMode: s.DeliveryMode, Priority: s.Priority,
		DeliveryTag: s.DeliveryTag, Redelivered: s.Redelivered}, nil
}

func quarantineCode(code string) bool {
	if len(code) == 0 || len(code) > 64 {
		return false
	}
	for _, c := range code {
		if c != '_' && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

func quarantineIdentity(source quarantineSnapshot) string {
	encoded, _ := quarantineStableContent(source)
	hash := sha256.Sum256(append([]byte("ichat:command-transport:v1\x00"), encoded...))
	return "mq:v1:" + hex.EncodeToString(hash[:])
}
