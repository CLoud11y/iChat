package rabbitmq

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func quarantineFixture() ConsumerMessage {
	return ConsumerMessage{Body: []byte{0xff, 0, '{'}, ContentType: "application/json", DeliveryMode: 2,
		MessageID: "not-a-command", CorrelationID: "physical-attempt", Timestamp: time.Unix(1700000000, 0).UTC(),
		Exchange: CommandsExchange, RoutingKey: SubmitRoutingKey, ConsumerTag: "consumer-1", DeliveryTag: 7,
		Headers: amqp.Table{"trace": []byte{0, 0xff}, "count": int64(9007199254740993)}}
}

func makeQuarantine(t *testing.T, message ConsumerMessage) QuarantineEnvelope {
	t.Helper()
	q, err := NewQuarantineEnvelope(message, "INVALID_DELIVERY", time.Unix(1700000001, 0))
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestQuarantineRoundTripAndOwnership(t *testing.T) {
	m := quarantineFixture()
	m.ContentEncoding, m.ReplyTo, m.Expiration = "binary", "reply", "1000"
	m.Type, m.UserID, m.AppID, m.Priority, m.Redelivered = "type", "user", "app", 3, true
	m.MessageID = string([]byte{0xff, 0xfe})
	m.Headers["nested"] = amqp.Table{string([]byte{0xfe}): []any{nil, true, []byte{0xff}, "text", string([]byte{0xfe})}}
	q := makeQuarantine(t, m)
	decoded, err := DecodeQuarantineEnvelope(q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ErrorCode() != "INVALID_DELIVERY" || !decoded.ObservedAt().Equal(time.Unix(1700000001, 0)) {
		t.Fatal("lost diagnostic metadata")
	}
	got, err := decoded.Source()
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("roundtrip lost source: %v", err)
	}
	if raw, err := decoded.RawHeaders(); err != nil || !json.Valid(raw) {
		t.Fatal("headers are not JSON")
	}
	m.Body[0] = 1
	m.Headers["trace"].([]byte)[0] = 1
	m.Headers["nested"].(amqp.Table)["new"] = true
	got.Body[0] = 2
	got.Headers["trace"].([]byte)[0] = 2
	encoded := q.Encode()
	encoded[0] = 0
	again, _ := q.Source()
	if again.Body[0] != 0xff || again.Headers["trace"].([]byte)[0] != 0 || len(again.Headers["nested"].(amqp.Table)) != 1 {
		t.Fatal("snapshot aliases input or returned data")
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		printed := fmt.Sprintf(format, q)
		if strings.Contains(printed, "trace") || strings.Contains(printed, "physical-attempt") {
			t.Fatal("snapshot leaked audit data")
		}
	}
}

func TestQuarantineHeaderTypes(t *testing.T) {
	values := []any{nil, false, byte(255), int8(-128), int(-1), int16(-32768), int32(-123), int64(math.MinInt64),
		uint16(65535), uint32(math.MaxUint32), float32(-1.25), math.Inf(1), math.Copysign(0, -1),
		amqp.Decimal{Scale: 3, Value: -123}, time.Unix(1700000000, 0).UTC(), "", "\xff", []byte{}, []byte(nil),
		amqp.Table{}, amqp.Table(nil), []any{}, []any(nil), []any{amqp.Table{"binary": []byte{0, 255}}}}
	for i, value := range values {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			m := quarantineFixture()
			m.Headers = amqp.Table{"value": value}
			q := makeQuarantine(t, m)
			decoded, err := DecodeQuarantineEnvelope(q.Encode())
			if err != nil {
				t.Fatal(err)
			}
			got, err := decoded.Source()
			if err != nil || !reflect.DeepEqual(value, got.Headers["value"]) {
				t.Fatalf("lost %T: %v", value, err)
			}
		})
	}
	for _, value := range []any{math.Float64frombits(0x7ff8000000001234), math.Float32frombits(0x7fc01234)} {
		m := quarantineFixture()
		m.Headers = amqp.Table{"nan": value}
		q := makeQuarantine(t, m)
		got, _ := q.Source()
		switch v := value.(type) {
		case float64:
			if math.Float64bits(v) != math.Float64bits(got.Headers["nan"].(float64)) {
				t.Fatal("lost NaN bits")
			}
		case float32:
			if math.Float32bits(v) != math.Float32bits(got.Headers["nan"].(float32)) {
				t.Fatal("lost NaN bits")
			}
		}
	}
}

func TestQuarantineTransportIdentity(t *testing.T) {
	m := quarantineFixture()
	base := makeQuarantine(t, m)
	if base.TransportID() != "mq:v1:79876c9ee7ef9f7b5fe8bed20da774d887d05f35bc6b77344dbf89cd0fa91d7d" {
		t.Fatal("v1 transport identity changed; persisted audit keys require compatibility")
	}
	// A channel replacement and automatic DLX change only audit metadata.
	m.DeliveryTag, m.ConsumerTag, m.Redelivered = 100, "replacement", true
	m.Exchange, m.RoutingKey, m.Expiration = QuarantineExchange, QuarantineRoutingKey, ""
	m.Headers["x-delivery-count"] = int64(7)
	m.Headers["x-acquired-count"] = int64(11)
	m.Headers["x-death"] = []any{amqp.Table{"count": int64(3), "reason": "delivery_limit"}}
	for _, prefix := range []string{"x-first-death-", "x-last-death-"} {
		for _, key := range []string{"exchange", "queue", "reason"} {
			m.Headers[prefix+key] = "changed"
		}
	}
	if q := makeQuarantine(t, m); q.TransportID() != base.TransportID() || bytes.Equal(q.Encode(), base.Encode()) {
		t.Fatal("redelivery/DLX changed identity or discarded audit metadata")
	}
	// Broker-consumed routing/TTL properties stay in the snapshot, not the key.
	before := quarantineFixture()
	before.Expiration = "1000"
	before.Headers["CC"], before.Headers["BCC"] = []any{"a"}, []any{"b"}
	if makeQuarantine(t, before).TransportID() != base.TransportID() {
		t.Fatal("routing/TTL affected key")
	}
	// Map iteration/insertion order cannot change a persisted transport key.
	before = quarantineFixture()
	before.Headers = amqp.Table{"count": int64(9007199254740993), "trace": []byte{0, 0xff}}
	if makeQuarantine(t, before).TransportID() != base.TransportID() {
		t.Fatal("header order affected key")
	}
	q, err := NewQuarantineEnvelope(m, "CONSTRAINT_FAILURE", time.Unix(1800000000, 0))
	if err != nil || q.TransportID() != base.TransportID() {
		t.Fatal("diagnostic metadata changed identity")
	}
	mutations := map[string]func(*ConsumerMessage){
		"body":         func(m *ConsumerMessage) { m.Body = append(m.Body, 1) },
		"attempt":      func(m *ConsumerMessage) { m.CorrelationID = "another-attempt" },
		"message":      func(m *ConsumerMessage) { m.MessageID = "another-message" },
		"header value": func(m *ConsumerMessage) { m.Headers["count"] = int64(2) },
		"header type":  func(m *ConsumerMessage) { m.Headers["count"] = "9007199254740993" },
		"priority":     func(m *ConsumerMessage) { m.Priority++ },
	}
	for name, change := range mutations {
		t.Run(name, func(t *testing.T) {
			m := quarantineFixture()
			change(&m)
			if makeQuarantine(t, m).TransportID() == base.TransportID() {
				t.Fatal("immutable input collision")
			}
		})
	}
	// Missing business/attempt IDs still have a deterministic raw transport ID.
	m = ConsumerMessage{Body: []byte{0xff}}
	if makeQuarantine(t, m).TransportID() != makeQuarantine(t, m).TransportID() {
		t.Fatal("unstable fallback")
	}
}

func TestQuarantineRejectsUnsafeOrTamperedInput(t *testing.T) {
	type customInt int
	cyclic := amqp.Table{}
	cyclic["self"] = cyclic
	for _, value := range []any{make(chan int), uint64(1), customInt(2), cyclic, strings.Repeat("x", MaxQuarantineBytes+1)} {
		m := quarantineFixture()
		m.Headers["bad"] = value
		if q, err := NewQuarantineEnvelope(m, "INVALID_DELIVERY", time.Now()); !errors.Is(err, ErrQuarantineEnvelope) || q.TransportID() != "" {
			t.Fatal("unsafe value accepted")
		}
	}
	m := quarantineFixture()
	m.Body = make([]byte, MaxQuarantineBytes+1)
	if _, err := NewQuarantineEnvelope(m, "INVALID_DELIVERY", time.Now()); err == nil {
		t.Fatal("oversized body accepted")
	}
	m = quarantineFixture()
	m.Headers = amqp.Table{}
	for i := 0; i < 4097; i++ {
		m.Headers[fmt.Sprint(i)] = nil
	}
	if _, err := NewQuarantineEnvelope(m, "INVALID_DELIVERY", time.Now()); err == nil {
		t.Fatal("excessive header count accepted")
	}
	for _, code := range []string{"", "body-secret", "\xff", strings.Repeat("A", 65)} {
		if _, err := NewQuarantineEnvelope(quarantineFixture(), code, time.Now()); !errors.Is(err, ErrQuarantineEnvelope) {
			t.Fatal("bad code accepted")
		}
	}
	if _, err := NewQuarantineEnvelope(quarantineFixture(), "INVALID_DELIVERY", time.Time{}); err == nil {
		t.Fatal("zero observation accepted")
	}
	base := makeQuarantine(t, quarantineFixture()).Encode()
	variants := [][]byte{nil, []byte("null"), []byte(`{}`), append(append([]byte{}, base...), []byte(`{}`)...),
		bytes.Replace(base, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(base, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		bytes.Replace(base, []byte(`"version":1`), []byte(`"version":1,"unknown":0`), 1),
		bytes.Replace(base, []byte("mq:v1:"), []byte("mq:v2:"), 1),
		bytes.Replace(base, []byte(`"type":"int64"`), []byte(`"type":"uint64"`), 1),
		bytes.Repeat([]byte{' '}, MaxQuarantineBytes+1)}
	for i, input := range variants {
		if q, err := DecodeQuarantineEnvelope(input); !errors.Is(err, ErrQuarantineEnvelope) || q.TransportID() != "" {
			t.Fatalf("tamper %d accepted", i)
		}
	}
	if _, err := (QuarantineEnvelope{}).Source(); err == nil {
		t.Fatal("zero snapshot accepted")
	}
}

func FuzzDecodeQuarantineEnvelope(f *testing.F) {
	q, err := NewQuarantineEnvelope(quarantineFixture(), "INVALID_DELIVERY", time.Unix(1700000001, 0))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(q.Encode())
	f.Add([]byte(`{"version":1}`))
	f.Add([]byte{0xff, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		q, err := DecodeQuarantineEnvelope(data)
		if err != nil {
			if !errors.Is(err, ErrQuarantineEnvelope) || q.TransportID() != "" {
				t.Fatal("invalid decode returned partial responsibility")
			}
			return
		}
		if !bytes.Equal(data, q.Encode()) {
			t.Fatal("accepted noncanonical wire")
		}
		if _, err := q.Source(); err != nil {
			t.Fatal("accepted inaccessible source")
		}
		if len(q.TransportID()) != 70 {
			t.Fatal("invalid persisted transport key")
		}
	})
}
