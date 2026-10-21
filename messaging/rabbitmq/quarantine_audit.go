package rabbitmq

import (
	"bytes"
	"encoding/json"
	"io"
	"time"
)

// MaxQuarantineAuditBytes bounds the storage document, including base64 source,
// exact stable content and independently bounded wrapper delivery metadata.
const MaxQuarantineAuditBytes = 24 * 1024 * 1024

type quarantineAuditWire struct {
	Version       int                   `json:"audit_version"`
	Format        QuarantineInputFormat `json:"format"`
	ReceivedAt    time.Time             `json:"received_at"`
	Source        []byte                `json:"source_envelope"`
	Delivery      *quarantineSnapshot   `json:"delivery_metadata"`
	StableContent []byte                `json:"stable_content"`
}

// EncodeAudit retains the first complete observation for SQL audit/replay.
// Source remains canonical v1 bytes even when a JSON column normalizes the
// surrounding document. Wrapper body is reconstructed from Source, not copied.
func (q QuarantineInput) EncodeAudit() ([]byte, error) {
	if q.format != QuarantineWrapped && q.format != QuarantineDeadLetter {
		return nil, ErrQuarantineInput
	}
	stable, err := q.envelope.StableContent()
	if err != nil {
		return nil, ErrQuarantineInput
	}
	wire := quarantineAuditWire{Version: 1, Format: q.format, ReceivedAt: q.receivedAt, Source: q.envelope.Encode(), StableContent: stable}
	if q.format == QuarantineWrapped {
		wire.Delivery = &q.metadata
	}
	encoded, err := json.Marshal(wire)
	if err != nil || len(encoded) > MaxQuarantineAuditBytes {
		return nil, ErrQuarantineInput
	}
	return encoded, nil
}

// DecodeQuarantineAudit accepts JSON-column whitespace/key ordering changes,
// then revalidates all source, metadata, stable content and format invariants.
// It never grants ACK capability or authenticates a stored record's origin.
func DecodeQuarantineAudit(encoded []byte) (QuarantineInput, error) {
	if len(encoded) == 0 || len(encoded) > MaxQuarantineAuditBytes {
		return QuarantineInput{}, ErrQuarantineInput
	}
	var wire quarantineAuditWire
	d := json.NewDecoder(bytes.NewReader(encoded))
	d.DisallowUnknownFields()
	if err := d.Decode(&wire); err != nil || wire.Version != 1 {
		return QuarantineInput{}, ErrQuarantineInput
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return QuarantineInput{}, ErrQuarantineInput
	}
	source, err := DecodeQuarantineEnvelope(wire.Source)
	if err != nil {
		return QuarantineInput{}, ErrQuarantineInput
	}
	stable, err := source.StableContent()
	if err != nil || !bytes.Equal(stable, wire.StableContent) {
		return QuarantineInput{}, ErrQuarantineInput
	}
	var delivery ConsumerMessage
	switch wire.Format {
	case QuarantineWrapped:
		if wire.Delivery == nil || len(wire.Delivery.Body) != 0 {
			return QuarantineInput{}, ErrQuarantineInput
		}
		delivery, err = wire.Delivery.message()
		delivery.Body = source.Encode()
	case QuarantineDeadLetter:
		if wire.Delivery != nil {
			return QuarantineInput{}, ErrQuarantineInput
		}
		delivery, err = source.Source()
	default:
		return QuarantineInput{}, ErrQuarantineInput
	}
	if err != nil {
		return QuarantineInput{}, ErrQuarantineInput
	}
	result, err := ParseQuarantineInput(delivery, wire.ReceivedAt)
	if err != nil {
		return QuarantineInput{}, ErrQuarantineInput
	}
	// Reject ignored/noncanonical typed node fields and contradictory raw-DLX
	// observations without demanding canonical outer JSON from MySQL.
	canonical, err := result.EncodeAudit()
	normalized, normalizeErr := json.Marshal(wire)
	if err != nil || normalizeErr != nil || !bytes.Equal(canonical, normalized) {
		return QuarantineInput{}, ErrQuarantineInput
	}
	return result, nil
}
