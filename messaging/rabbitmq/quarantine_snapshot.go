package rabbitmq

import "encoding/json"

// Shared capture retains the existing v1 byte/type representation and budgets.
func snapshotQuarantineMessage(source ConsumerMessage) (quarantineSnapshot, error) {
	texts := [12]string{source.ContentType, source.ContentEncoding, source.CorrelationID, source.ReplyTo,
		source.Expiration, source.MessageID, source.Type, source.UserID, source.AppID,
		source.ConsumerTag, source.Exchange, source.RoutingKey}
	size := len(source.Body)
	for _, value := range texts {
		size += len(value)
	}
	if size > MaxQuarantineBytes {
		return quarantineSnapshot{}, ErrQuarantineEnvelope
	}
	headers, err := encodeQuarantineHeader(source.Headers, 0, &quarantineBudget{bytes: size})
	if err != nil {
		return quarantineSnapshot{}, err
	}
	stamp, err := source.Timestamp.UTC().MarshalBinary()
	if err != nil {
		return quarantineSnapshot{}, ErrQuarantineEnvelope
	}
	snapshot := quarantineSnapshot{Body: append([]byte{}, source.Body...), Headers: headers, Timestamp: stamp,
		DeliveryMode: source.DeliveryMode, Priority: source.Priority, DeliveryTag: source.DeliveryTag, Redelivered: source.Redelivered}
	for i, value := range texts {
		snapshot.Text[i] = []byte(value)
	}
	return snapshot, nil
}

func quarantineStableContent(source quarantineSnapshot) ([]byte, error) {
	// These v1 exclusions must remain identical to transport identity generation.
	source.Text[4], source.Text[9], source.Text[10], source.Text[11] = nil, nil, nil, nil
	source.DeliveryTag, source.Redelivered = 0, false
	filtered := make([]quarantineHeaderField, 0, len(source.Headers.Fields))
	for _, field := range source.Headers.Fields {
		switch string(field.Key) {
		case "x-death", "x-delivery-count", "x-acquired-count", "x-first-death-exchange", "x-first-death-queue", "x-first-death-reason",
			"x-last-death-exchange", "x-last-death-queue", "x-last-death-reason", "CC", "BCC":
			continue
		}
		filtered = append(filtered, field)
	}
	source.Headers = quarantineHeader{Type: "table", Fields: filtered}
	return json.Marshal(source)
}

// StableContent supplies exact comparison material for a future audit writer.
// It does not replace storage validation, authentication or a SQL transaction.
func (q QuarantineEnvelope) StableContent() ([]byte, error) {
	var wire quarantineWire
	if len(q.encoded) == 0 || json.Unmarshal(q.encoded, &wire) != nil {
		return nil, ErrQuarantineEnvelope
	}
	return quarantineStableContent(wire.Source)
}
