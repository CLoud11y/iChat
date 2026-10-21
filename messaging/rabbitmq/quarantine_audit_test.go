package rabbitmq

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestQuarantineAuditRoundTrip(t *testing.T) {
	_, valid := quarantineValidSource(t)
	for _, message := range []ConsumerMessage{quarantineWrappedInput(t, quarantineFixture(), "INVALID_DELIVERY"), quarantineWrappedInput(t, valid, "DATA_CONSTRAINT"), quarantineDLXInput(t, quarantineFixture(), "expired"), quarantineDLXInput(t, valid, "rejected")} {
		input, err := ParseQuarantineInput(message, quarantineInputTime)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := input.EncodeAudit()
		if err != nil {
			t.Fatal(err)
		}
		// SQL JSON can reorder object keys and add whitespace. The canonical
		// source envelope and precise stable content must survive as bytes.
		var object map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &object); err != nil {
			t.Fatal(err)
		}
		normalized, err := json.MarshalIndent(object, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeQuarantineAudit(normalized)
		if err != nil {
			t.Fatal(err)
		}
		again, _ := got.EncodeAudit()
		if !bytes.Equal(encoded, again) || !bytes.Equal(got.Envelope().Encode(), input.Envelope().Encode()) || got.Format() != input.Format() || !got.ReceivedAt().Equal(input.ReceivedAt()) {
			t.Fatal("audit round trip changed observation")
		}
		a, aok := got.Identity()
		b, bok := input.Identity()
		if a != b || aok != bok {
			t.Fatal("identity changed")
		}
		encoded[0] = 0
		if _, err := got.EncodeAudit(); err != nil {
			t.Fatal("stored result aliases document", err)
		}
	}
}

func TestQuarantineAuditRejectsCorruption(t *testing.T) {
	message := quarantineWrappedInput(t, quarantineFixture(), "INVALID_DELIVERY")
	input, _ := ParseQuarantineInput(message, quarantineInputTime)
	encoded, _ := input.EncodeAudit()
	for name, mutate := range map[string]func(*quarantineAuditWire){
		"version":                func(w *quarantineAuditWire) { w.Version = 2 },
		"format":                 func(w *quarantineAuditWire) { w.Format = "guess" },
		"source":                 func(w *quarantineAuditWire) { w.Source[0] = 0 },
		"comparison":             func(w *quarantineAuditWire) { w.StableContent[0] = 0 },
		"observation":            func(w *quarantineAuditWire) { w.ReceivedAt = time.Time{} },
		"metadata missing":       func(w *quarantineAuditWire) { w.Delivery = nil },
		"metadata body":          func(w *quarantineAuditWire) { w.Delivery.Body = []byte("duplicate or forged") },
		"metadata route":         func(w *quarantineAuditWire) { w.Delivery.Text[10] = []byte("foreign") },
		"metadata type":          func(w *quarantineAuditWire) { w.Delivery.Headers.Type = "unknown" },
		"metadata ignored field": func(w *quarantineAuditWire) { w.Delivery.Headers.Data = []byte("ignored") },
	} {
		t.Run(name, func(t *testing.T) {
			var wire quarantineAuditWire
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			mutate(&wire)
			bad, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			q, err := DecodeQuarantineAudit(bad)
			if !errors.Is(err, ErrQuarantineInput) || q.Format() != "" {
				t.Fatal("corrupt audit accepted", err)
			}
		})
	}
	for _, bad := range [][]byte{nil, append(append([]byte{}, encoded...), []byte(` {}`)...), bytes.Replace(encoded, []byte(`"audit_version":1`), []byte(`"unknown":1,"audit_version":1`), 1), bytes.Repeat([]byte{' '}, MaxQuarantineAuditBytes+1)} {
		if _, err := DecodeQuarantineAudit(bad); !errors.Is(err, ErrQuarantineInput) {
			t.Fatal(err)
		}
	}
	if _, err := (QuarantineInput{}).EncodeAudit(); !errors.Is(err, ErrQuarantineInput) {
		t.Fatal(err)
	}
	raw, _ := ParseQuarantineInput(quarantineDLXInput(t, quarantineFixture(), "rejected"), quarantineInputTime)
	data, _ := raw.EncodeAudit()
	var wire quarantineAuditWire
	_ = json.Unmarshal(data, &wire)
	wire.ReceivedAt = wire.ReceivedAt.Add(time.Second)
	data, _ = json.Marshal(wire)
	if _, err := DecodeQuarantineAudit(data); !errors.Is(err, ErrQuarantineInput) {
		t.Fatal("raw observation contradicted source", err)
	}
}

func FuzzDecodeQuarantineAudit(f *testing.F) {
	q, err := NewQuarantineEnvelope(quarantineFixture(), "INVALID_DELIVERY", quarantineInputTime)
	if err != nil {
		f.Fatal(err)
	}
	input, err := ParseQuarantineInput(ConsumerMessage{Body: q.Encode(), ContentType: QuarantineContentType, Type: QuarantineMessageType, DeliveryMode: 2, MessageID: q.TransportID(), CorrelationID: "0123456789abcdef0123456789abcdef", Exchange: QuarantineExchange, RoutingKey: QuarantineRoutingKey}, quarantineInputTime)
	if err != nil {
		f.Fatal(err)
	}
	encoded, _ := input.EncodeAudit()
	f.Add(encoded)
	f.Add([]byte(`{"audit_version":2}`))
	f.Add([]byte{0xff, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		input, err := DecodeQuarantineAudit(data)
		if err != nil {
			if !errors.Is(err, ErrQuarantineInput) {
				t.Fatal(err)
			}
			return
		}
		again, err := input.EncodeAudit()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeQuarantineAudit(again); err != nil {
			t.Fatal(err)
		}
	})
}
