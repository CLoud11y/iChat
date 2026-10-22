package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging"
	"iChat/messaging/rabbitmq"

	amqp "github.com/rabbitmq/amqp091-go"
)

func adminQuarantineInput(t *testing.T, label string, valid, dlx bool, clientIDs ...string) rabbitmq.QuarantineInput {
	t.Helper()
	stamp := time.Date(2026, 10, 22, 1, 2, 3, 123456000, time.UTC)
	source := rabbitmq.ConsumerMessage{Body: []byte{0xff, 0, '{'}, ContentType: "application/json", DeliveryMode: 2,
		Exchange: rabbitmq.CommandsExchange, RoutingKey: rabbitmq.SubmitRoutingKey, MessageID: label, CorrelationID: "original-" + label,
		Headers: amqp.Table{"binary": []byte{0, 255}, "large": int64(9007199254740993), "decimal": amqp.Decimal{Scale: 2, Value: -12}}}
	if valid {
		clientID := "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1"
		if len(clientIDs) != 0 {
			clientID = clientIDs[0]
		}
		command, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: clientID, TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: label + "中🙂\x00"})
		if err != nil {
			t.Fatal(err)
		}
		wire, err := messaging.NewEnvelope(command, stamp)
		if err != nil {
			t.Fatal(err)
		}
		source.Body, err = json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		source.MessageID, source.CorrelationID = wire.CommandID, wire.PublishAttemptID
	}
	q, err := rabbitmq.NewQuarantineEnvelope(source, "DATA_CONSTRAINT", stamp.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	delivery := rabbitmq.ConsumerMessage{Body: q.Encode(), ContentType: rabbitmq.QuarantineContentType, Type: rabbitmq.QuarantineMessageType, DeliveryMode: 2,
		Exchange: rabbitmq.QuarantineExchange, RoutingKey: rabbitmq.QuarantineRoutingKey, MessageID: q.TransportID(), CorrelationID: "0123456789abcdef0123456789abcdef"}
	if dlx {
		delivery = source
		delivery.Exchange, delivery.RoutingKey = rabbitmq.QuarantineExchange, rabbitmq.QuarantineRoutingKey
		delivery.Headers["x-death"] = []any{amqp.Table{"queue": rabbitmq.PersistQueue, "exchange": rabbitmq.CommandsExchange, "routing-keys": []any{rabbitmq.SubmitRoutingKey}, "reason": "rejected", "count": int64(1), "time": stamp}}
		delivery.Headers["x-last-death-queue"], delivery.Headers["x-last-death-exchange"], delivery.Headers["x-last-death-reason"] = rabbitmq.PersistQueue, rabbitmq.CommandsExchange, "rejected"
	}
	input, err := rabbitmq.ParseQuarantineInput(delivery, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

type adminQuarantineReader struct {
	options database.QuarantineListOptions
	id      string
	detail  database.QuarantineDetail
	err     error
}

func (r *adminQuarantineReader) List(ctx context.Context, options database.QuarantineListOptions) (database.QuarantinePage, error) {
	if _, ok := ctx.Deadline(); !ok {
		return database.QuarantinePage{}, errors.New("missing deadline")
	}
	r.options = options
	return database.QuarantinePage{Records: []database.QuarantineSummary{r.detail.Summary}}, r.err
}

func (r *adminQuarantineReader) Get(_ context.Context, id string) (database.QuarantineDetail, error) {
	r.id = id
	return r.detail, r.err
}

func TestQuarantineCLISelectionAndOutput(t *testing.T) {
	input := adminQuarantineInput(t, "private-source", false, false)
	id := input.Envelope().TransportID()
	reader := &adminQuarantineReader{detail: database.QuarantineDetail{Summary: database.QuarantineSummary{TransportID: id, State: "open", ErrorCode: "DATA_CONSTRAINT"}, Input: input}}
	closed := 0
	open := func(string) (quarantineReader, func() error, error) {
		return reader, func() error { closed++; return nil }, nil
	}
	for _, args := range [][]string{{"list"}, {"list", "--state", "all", "--after", id, "--limit", "1"}, {"show", "--transport-id", id}, {"show", "--transport-id", id, "--include-raw"}} {
		var output bytes.Buffer
		if err := runQuarantine(args, &output, open); err != nil {
			t.Fatal(err)
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if args[0] == "list" {
			if string(result["audit_verified"]) != "false" || reader.options.Limit < 1 {
				t.Fatal("list claimed full verification or lost limit")
			}
			continue
		}
		if reader.id != id || string(result["audit_verified"]) != "true" {
			t.Fatal("selection or verification lost")
		}
		_, included := result["audit"]
		if included != (len(args) == 4) {
			t.Fatal("raw audit inclusion is not explicit")
		}
		if included {
			var body []byte
			if err := json.Unmarshal(result["raw_body_base64"], &body); err != nil || !bytes.Equal(body, []byte{0xff, 0, '{'}) {
				t.Fatal("raw bytes changed", err)
			}
			stored, err := rabbitmq.DecodeQuarantineAudit(result["audit"])
			if err != nil || stored.Envelope().TransportID() != id {
				t.Fatal("typed audit changed", err)
			}
		} else if _, ok := result["raw_body_base64"]; ok || bytes.Contains(output.Bytes(), []byte("private-source")) {
			t.Fatal("default view leaked raw payload")
		}
	}
	if closed != 4 {
		t.Fatal("reader ownership leaked", closed)
	}
	reader.err = database.ErrQuarantineInvariant
	var output bytes.Buffer
	if err := runQuarantine([]string{"show", "--transport-id", id}, &output, open); !errors.Is(err, database.ErrQuarantineInvariant) || output.Len() != 0 || closed != 5 {
		t.Fatal("failed query output or cleanup", err)
	}
	reader.err = nil
	if err := runQuarantine([]string{"list"}, adminBrokenWriter{}, open); err == nil || strings.Contains(err.Error(), "secret") || closed != 6 {
		t.Fatal("writer failure or ownership", err)
	}
	if err := runQuarantine([]string{"list"}, io.Discard, func(string) (quarantineReader, func() error, error) {
		return reader, func() error { return errors.New("close failed") }, nil
	}); err == nil {
		t.Fatal("close error ignored")
	}
}

type adminBrokenWriter struct{}

func (adminBrokenWriter) Write([]byte) (int, error) { return 0, errors.New("secret writer failure") }

func TestQuarantineCLIRejectsBeforeOpen(t *testing.T) {
	for _, args := range [][]string{nil, {"replay"}, {"resolve"}, {"show"}, {"show", "--transport-id", "secret"}, {"list", "--limit", "0"}, {"list", "--limit", "101"}, {"list", "--state", "secret"}, {"list", "--after", "secret"}, {"list", "--include-raw"}, {"list", "extra"}, {"list", "--unknown=secret"}} {
		called := false
		err := runQuarantine(args, io.Discard, func(string) (quarantineReader, func() error, error) { called = true; return nil, nil, nil })
		if called || err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid selection opened SQL or leaked input", args, err)
		}
	}
}

func TestQuarantineSQLConfiguration(t *testing.T) {
	for _, key := range []string{"USER", "PASSWORD", "HOST", "PORT", "DBNAME"} {
		t.Setenv("ICHAT_MYSQL_"+key, "")
	}
	t.Setenv("ICHAT_MYSQL_USER", "fixture")
	t.Setenv("ICHAT_MYSQL_PASSWORD", "secret:/@中文")
	t.Setenv("ICHAT_MYSQL_HOST", "::1")
	t.Setenv("ICHAT_MYSQL_PORT", "3307")
	t.Setenv("ICHAT_MYSQL_DBNAME", "fixture")
	options, err := loadQuarantineMySQL("")
	if err != nil || options.Addr != "[::1]:3307" || options.Passwd != "secret:/@中文" || options.Timeout != 3*time.Second {
		t.Fatal("SQL environment configuration failed", err)
	}
	file := filepath.Join(t.TempDir(), "sql.yml")
	if err := os.WriteFile(file, []byte("MYSQL:\n  user: ignored\n  password: ignored\n  host: ignored\n  port: 3306\n  dbname: ignored\nRUNTIME:\n  role: invalid\nRABBITMQ: invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	options, err = loadQuarantineMySQL(file)
	if err != nil || options.User != "fixture" || options.Passwd != "secret:/@中文" {
		t.Fatal("SQL override or unrelated config handling failed", err)
	}
	t.Setenv("ICHAT_MYSQL_USER", "")
	if _, err := loadQuarantineMySQL(file); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("empty override adopted file value or leaked credentials")
	}
	if _, err := loadQuarantineMySQL(filepath.Join(t.TempDir(), "secret.yml")); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("configuration error leaked input")
	}
}
