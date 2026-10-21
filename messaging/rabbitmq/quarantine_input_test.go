package rabbitmq

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"iChat/messaging"

	amqp "github.com/rabbitmq/amqp091-go"
)

var quarantineInputTime = time.Unix(1700000002, 0).UTC()

func quarantineWrappedInput(t *testing.T, source ConsumerMessage, code string) ConsumerMessage {
	t.Helper()
	q, err := NewQuarantineEnvelope(source, code, quarantineInputTime.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return ConsumerMessage{Body: q.Encode(), ContentType: QuarantineContentType, Type: QuarantineMessageType, DeliveryMode: amqp.Persistent,
		MessageID: q.TransportID(), CorrelationID: "0123456789abcdef0123456789abcdef", Exchange: QuarantineExchange, RoutingKey: QuarantineRoutingKey,
		Headers: amqp.Table{"outer-trace": []byte{0, 255}, "x-acquired-count": int64(1)}, ConsumerTag: "audit", DeliveryTag: 17}
}

func quarantineDLXInput(t *testing.T, source ConsumerMessage, reason string) ConsumerMessage {
	t.Helper()
	q, err := NewQuarantineEnvelope(source, "INVALID_DELIVERY", quarantineInputTime)
	if err != nil {
		t.Fatal(err)
	}
	d, err := q.Source()
	if err != nil {
		t.Fatal(err)
	}
	if d.Headers == nil {
		d.Headers = amqp.Table{}
	}
	d.Headers["x-death"] = []any{amqp.Table{"queue": PersistQueue, "exchange": CommandsExchange, "routing-keys": []any{SubmitRoutingKey}, "reason": reason, "count": int64(2), "time": quarantineInputTime}}
	for _, prefix := range []string{"x-first-death-", "x-last-death-"} {
		d.Headers[prefix+"queue"] = PersistQueue
		d.Headers[prefix+"exchange"] = CommandsExchange
		d.Headers[prefix+"reason"] = reason
	}
	d.Headers["x-acquired-count"] = int64(3)
	d.Expiration = ""
	d.Exchange, d.RoutingKey = QuarantineExchange, QuarantineRoutingKey
	d.ConsumerTag, d.DeliveryTag, d.Redelivered = "audit-raw", 21, true
	return d
}

func quarantineValidSource(t *testing.T) (messaging.Command, ConsumerMessage) {
	t.Helper()
	c := publisherCommand(t)
	e, err := messaging.NewEnvelope(c, quarantineInputTime)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return c, ConsumerMessage{Body: body, ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageID: c.CommandID, CorrelationID: e.PublishAttemptID, Exchange: CommandsExchange, RoutingKey: SubmitRoutingKey}
}

func TestQuarantineInputFormatsStableContentAndOwnership(t *testing.T) {
	source := quarantineFixture()
	source.Expiration = "1000"
	wrapper := quarantineWrappedInput(t, source, "INVALID_DELIVERY")
	dlx := quarantineDLXInput(t, source, "rejected")
	a, err := ParseQuarantineInput(wrapper, quarantineInputTime)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseQuarantineInput(dlx, quarantineInputTime.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if a.Format() != QuarantineWrapped || b.Format() != QuarantineDeadLetter || !a.ReceivedAt().Equal(quarantineInputTime) || a.Envelope().ErrorCode() != "INVALID_DELIVERY" || b.Envelope().ErrorCode() != "BROKER_REJECTED" {
		t.Fatal("wrong audit format/observation")
	}
	as, _ := a.Envelope().StableContent()
	bs, _ := b.Envelope().StableContent()
	if !bytes.Equal(as, bs) || a.Envelope().TransportID() != b.Envelope().TransportID() {
		t.Fatal("active and DLX stable content disagree")
	}
	hash := sha256.Sum256(append([]byte("ichat:command-transport:v1\x00"), as...))
	if a.Envelope().TransportID() != "mq:v1:"+hex.EncodeToString(hash[:]) {
		t.Fatal("stable comparison bytes diverged from v1 key")
	}
	for _, input := range []QuarantineInput{a, b} {
		if identity, ok := input.Identity(); ok || identity.CommandID != "" {
			t.Fatal("malformed body gained business identity")
		}
	}
	got, err := a.Delivery()
	if err != nil || !reflect.DeepEqual(got, wrapper) {
		t.Fatal("wrapper delivery lost metadata", err)
	}
	got.Body[0] = 0
	got.Headers["outer-trace"].([]byte)[0] = 1
	wrapper.Body[0] = 0
	wrapper.Headers["outer-trace"].([]byte)[0] = 2
	as[0] = 0
	again, _ := a.Delivery()
	stable, _ := a.Envelope().StableContent()
	if again.Body[0] != '{' || again.Headers["outer-trace"].([]byte)[0] != 0 || stable[0] != '{' {
		t.Fatal("immutable parser result aliases input/accessors")
	}
	raw, err := b.Delivery()
	if err != nil || !reflect.DeepEqual(raw, dlx) {
		t.Fatal("raw DLX metadata was rewritten", err)
	}
	raw.Headers["x-death"].([]any)[0].(amqp.Table)["reason"] = "changed"
	again, _ = b.Delivery()
	if again.Headers["x-death"].([]any)[0].(amqp.Table)["reason"] != "rejected" {
		t.Fatal("nested audit headers alias")
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if printed := fmt.Sprintf(format, a); strings.Contains(printed, "outer-trace") || strings.Contains(printed, "physical-attempt") {
			t.Fatal("audit formatting leaked raw input")
		}
	}
}

func TestQuarantineInputBusinessIdentityIsValidatedEnrichment(t *testing.T) {
	command, source := quarantineValidSource(t)
	for _, delivery := range []ConsumerMessage{quarantineWrappedInput(t, source, "DATA_CONSTRAINT"), quarantineDLXInput(t, source, "delivery_limit")} {
		q, err := ParseQuarantineInput(delivery, quarantineInputTime)
		if err != nil {
			t.Fatal(err)
		}
		identity, ok := q.Identity()
		if !ok || identity.SenderID != command.SenderID || identity.CommandID != command.CommandID || identity.ClientMessageID != command.ClientMessageID || identity.PayloadHash != command.PayloadHash {
			t.Fatal("verified command identity lost")
		}
		original, _ := q.Envelope().Source()
		if q.Format() == QuarantineDeadLetter && original.Exchange != QuarantineExchange {
			t.Fatal("enrichment rewrote saved source route")
		}
	}
	mutations := map[string]func(*ConsumerMessage){
		"message id":   func(m *ConsumerMessage) { m.MessageID = "spoof" },
		"attempt":      func(m *ConsumerMessage) { m.CorrelationID = "spoof" },
		"content type": func(m *ConsumerMessage) { m.ContentType = "text/plain" },
		"source route": func(m *ConsumerMessage) { m.Exchange = "other" },
		"body hash": func(m *ConsumerMessage) {
			m.Body = bytes.Replace(m.Body, []byte(command.PayloadHash), []byte(strings.Repeat("0", 64)), 1)
		},
		"sender": func(m *ConsumerMessage) {
			m.Body = bytes.Replace(m.Body, []byte(`"senderId":"12"`), []byte(`"senderId":"999"`), 1)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			_, m := quarantineValidSource(t)
			mutate(&m)
			q, err := ParseQuarantineInput(quarantineWrappedInput(t, m, "INVALID_DELIVERY"), quarantineInputTime)
			if err != nil {
				t.Fatal("bad business wire must still remain auditable", err)
			}
			if _, ok := q.Identity(); ok {
				t.Fatal("unverified source supplied query identity")
			}
		})
	}
}

func TestQuarantineInputDeadLetterReasonsAndHistory(t *testing.T) {
	for reason, code := range map[string]string{"rejected": "BROKER_REJECTED", "expired": "BROKER_EXPIRED", "maxlen": "BROKER_MAXLEN", "delivery_limit": "BROKER_DELIVERY_LIMIT"} {
		t.Run(reason, func(t *testing.T) {
			m := quarantineDLXInput(t, quarantineFixture(), reason)
			q, err := ParseQuarantineInput(m, quarantineInputTime)
			if err != nil || q.Envelope().ErrorCode() != code {
				t.Fatal("reason not mapped", err)
			}
		})
	}
	m := quarantineDLXInput(t, quarantineFixture(), "rejected")
	history := m.Headers["x-death"].([]any)
	older := amqp.Table{"queue": "earlier-queue", "exchange": "earlier-exchange", "routing-keys": []any{"earlier"}, "reason": "expired", "count": int64(1), "time": quarantineInputTime.Add(-time.Hour)}
	m.Headers["x-death"] = append(history, older)
	if _, err := ParseQuarantineInput(m, quarantineInputTime); err != nil {
		t.Fatal("valid older death history rejected", err)
	}
}

func TestQuarantineInputRejectsAmbiguousOrTamperedFormats(t *testing.T) {
	tests := map[string]func(*ConsumerMessage){
		"wrong route":    func(m *ConsumerMessage) { m.Exchange = CommandsExchange },
		"wrong key":      func(m *ConsumerMessage) { m.RoutingKey = SubmitRoutingKey },
		"partial marker": func(m *ConsumerMessage) { m.Type = "" },
		"other marker":   func(m *ConsumerMessage) { m.ContentType = "application/json" },
		"mode":           func(m *ConsumerMessage) { m.DeliveryMode = 1 },
		"encoding":       func(m *ConsumerMessage) { m.ContentEncoding = "gzip" },
		"ttl":            func(m *ConsumerMessage) { m.Expiration = "1000" },
		"attempt":        func(m *ConsumerMessage) { m.CorrelationID = "old-attempt" },
		"transport id":   func(m *ConsumerMessage) { m.MessageID = "spoof" },
		"corrupt body":   func(m *ConsumerMessage) { m.Body = []byte("private invalid wrapper") },
		"future version": func(m *ConsumerMessage) {
			m.Body = bytes.Replace(m.Body, []byte(`"version":1`), []byte(`"version":2`), 1)
		},
		"unknown code": func(m *ConsumerMessage) {
			q, err := NewQuarantineEnvelope(quarantineFixture(), "FUTURE_CODE", quarantineInputTime)
			if err != nil {
				t.Fatal(err)
			}
			m.Body = q.Encode()
			m.MessageID = q.TransportID()
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			m := quarantineWrappedInput(t, quarantineFixture(), "INVALID_DELIVERY")
			// Even full-looking DLX evidence cannot rescue a malformed wrapper.
			m.Headers = quarantineDLXInput(t, quarantineFixture(), "rejected").Headers
			mutate(&m)
			assertQuarantineInputInvalid(t, m)
		})
	}
}

func assertQuarantineInputInvalid(t *testing.T, m ConsumerMessage) {
	t.Helper()
	q, err := ParseQuarantineInput(m, quarantineInputTime)
	if !errors.Is(err, ErrQuarantineInput) || q.Format() != "" || q.Envelope().TransportID() != "" || !q.ReceivedAt().IsZero() {
		t.Fatal("invalid input returned partial audit authority", err)
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, err), "private") {
			t.Fatal("input error leaked raw body")
		}
	}
}

func TestQuarantineInputRejectsUnsafeDeathEvidenceAndBudgets(t *testing.T) {
	mutations := map[string]func(*ConsumerMessage){
		"absent":           func(m *ConsumerMessage) { delete(m.Headers, "x-death") },
		"wrong array":      func(m *ConsumerMessage) { m.Headers["x-death"] = "private wrong type" },
		"foreign queue":    func(m *ConsumerMessage) { m.Headers["x-death"].([]any)[0].(amqp.Table)["queue"] = "foreign" },
		"foreign exchange": func(m *ConsumerMessage) { m.Headers["x-death"].([]any)[0].(amqp.Table)["exchange"] = "foreign" },
		"foreign routing": func(m *ConsumerMessage) {
			m.Headers["x-death"].([]any)[0].(amqp.Table)["routing-keys"] = []any{"foreign"}
		},
		"unknown reason":     func(m *ConsumerMessage) { m.Headers["x-death"].([]any)[0].(amqp.Table)["reason"] = "future" },
		"zero count":         func(m *ConsumerMessage) { m.Headers["x-death"].([]any)[0].(amqp.Table)["count"] = int64(0) },
		"coerced count":      func(m *ConsumerMessage) { m.Headers["x-death"].([]any)[0].(amqp.Table)["count"] = float64(2) },
		"no time":            func(m *ConsumerMessage) { delete(m.Headers["x-death"].([]any)[0].(amqp.Table), "time") },
		"contradictory last": func(m *ConsumerMessage) { m.Headers["x-last-death-reason"] = "expired" },
		"bad older event":    func(m *ConsumerMessage) { m.Headers["x-death"] = append(m.Headers["x-death"].([]any), nil) },
		"oversize body":      func(m *ConsumerMessage) { m.Body = make([]byte, MaxQuarantineBytes+1) },
		"unsupported header": func(m *ConsumerMessage) { m.Headers["unsupported"] = make(chan int) },
		"cycle":              func(m *ConsumerMessage) { m.Headers["cycle"] = m.Headers },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			m := quarantineDLXInput(t, quarantineFixture(), "rejected")
			mutate(&m)
			assertQuarantineInputInvalid(t, m)
		})
	}
	m := quarantineWrappedInput(t, quarantineFixture(), "INVALID_DELIVERY")
	if _, err := ParseQuarantineInput(m, time.Time{}); !errors.Is(err, ErrQuarantineInput) {
		t.Fatal("zero audit observation accepted")
	}
	if _, err := (QuarantineInput{}).Delivery(); err == nil {
		t.Fatal("zero input exposed delivery")
	}
	if _, err := (QuarantineEnvelope{}).StableContent(); err == nil {
		t.Fatal("zero envelope exposed stable content")
	}
}

func TestQuarantineInputLargeWrapperKeepsIndependentMetadataBudget(t *testing.T) {
	source := quarantineFixture()
	source.Body = bytes.Repeat([]byte{255}, 2*1024*1024)
	m := quarantineWrappedInput(t, source, "INVALID_DELIVERY")
	m.Headers["outer-large"] = make([]byte, 1536*1024)
	if len(m.Body)+len(m.Headers["outer-large"].([]byte)) <= MaxQuarantineBytes {
		t.Fatal("fixture did not cross a single body budget")
	}
	q, err := ParseQuarantineInput(m, quarantineInputTime)
	if err != nil {
		t.Fatal("valid large wrapper lost due to double body encoding", err)
	}
	d, err := q.Delivery()
	if err != nil || !bytes.Equal(d.Body, m.Body) || len(d.Headers["outer-large"].([]byte)) != 1536*1024 {
		t.Fatal("large observation lost metadata", err)
	}
}

func FuzzParseQuarantineInput(f *testing.F) {
	source := quarantineFixture()
	q, err := NewQuarantineEnvelope(source, "INVALID_DELIVERY", quarantineInputTime)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(q.Encode(), true)
	f.Add([]byte{255, 0}, false)
	f.Add([]byte(`{"version":2}`), true)
	f.Fuzz(func(t *testing.T, body []byte, wrapped bool) {
		var m ConsumerMessage
		if wrapped {
			m = quarantineWrappedInput(t, source, "INVALID_DELIVERY")
		} else {
			m = quarantineDLXInput(t, source, "rejected")
		}
		m.Body = body
		input, err := ParseQuarantineInput(m, quarantineInputTime)
		if err != nil {
			if !errors.Is(err, ErrQuarantineInput) || input.Format() != "" || input.Envelope().TransportID() != "" {
				t.Fatal("partial/foreign parse failure")
			}
			return
		}
		d, err := input.Delivery()
		if err != nil || !bytes.Equal(d.Body, body) {
			t.Fatal("accepted input lost delivery bytes", err)
		}
		stable, err := input.Envelope().StableContent()
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(append([]byte("ichat:command-transport:v1\x00"), stable...))
		if input.Envelope().TransportID() != "mq:v1:"+hex.EncodeToString(hash[:]) {
			t.Fatal("identity mismatch")
		}
	})
}
