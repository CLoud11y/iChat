package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"iChat/messaging"
	"iChat/messaging/rabbitmq"

	mysql "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
)

// A private driver verifies database/sql's real commit/rollback boundaries;
// the integration suite separately exercises MySQL locks and uniqueness.
type quarantineTestState struct {
	stage    string
	cause    error
	existing bool
	row      []driver.Value
	events   []string
	mutate   func([]driver.Value)
}
type quarantineTestConnector struct{ state *quarantineTestState }

func (c quarantineTestConnector) Driver() driver.Driver { return commandTestDriver{} }
func (c quarantineTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &quarantineTestConn{state: c.state}, nil
}

type quarantineTestConn struct{ state *quarantineTestState }

func (*quarantineTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*quarantineTestConn) Close() error { return nil }
func (c *quarantineTestConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *quarantineTestConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.state.events = append(c.state.events, "begin")
	if c.state.stage == "begin" {
		return nil, c.state.cause
	}
	return &quarantineTestTx{c.state}, nil
}
func (c *quarantineTestConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	s := c.state
	s.events = append(s.events, "insert")
	if query != insertQuarantineAuditSQL {
		return nil, errors.New("unexpected SQL mutation")
	}
	if s.stage == "insert" {
		return nil, s.cause
	}
	if s.existing {
		return nil, &mysql.MySQLError{Number: 1062, Message: "private duplicate details"}
	}
	s.row = []driver.Value{args[0].Value, args[1].Value, args[2].Value, args[3].Value, args[4].Value, args[5].Value, args[6].Value, args[7].Value, args[8].Value, "open", nil, args[9].Value, nil}
	if s.stage == "affected" {
		return commandTestResult{err: s.cause}, nil
	}
	if s.stage == "rowcount" {
		return commandTestResult{count: 0}, nil
	}
	return commandTestResult{count: 1}, nil
}
func (c *quarantineTestConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	s := c.state
	s.events = append(s.events, "read")
	if query != readQuarantineAuditSQL {
		return nil, errors.New("expected locking read")
	}
	if s.stage == "read" {
		return nil, s.cause
	}
	if s.mutate != nil {
		s.mutate(s.row)
	}
	if s.stage == "missing" {
		return &quarantineTestRows{}, nil
	}
	return &quarantineTestRows{row: s.row}, nil
}

type quarantineTestTx struct{ state *quarantineTestState }

func (tx *quarantineTestTx) Commit() error {
	tx.state.events = append(tx.state.events, "commit")
	if tx.state.stage == "commit" {
		return tx.state.cause
	}
	return nil
}
func (tx *quarantineTestTx) Rollback() error {
	tx.state.events = append(tx.state.events, "rollback")
	if tx.state.stage == "rollback" {
		return tx.state.cause
	}
	return nil
}

type quarantineTestRows struct {
	row  []driver.Value
	done bool
}

func (*quarantineTestRows) Columns() []string {
	return strings.Split("id,sender,client,command,hash,body,document,code,detail,state,replay,created,resolved", ",")
}
func (*quarantineTestRows) Close() error { return nil }
func (r *quarantineTestRows) Next(values []driver.Value) error {
	if r.done || r.row == nil {
		return io.EOF
	}
	r.done = true
	copy(values, r.row)
	return nil
}

func newQuarantineTestRepository(t *testing.T, s *quarantineTestState) *QuarantineRepository {
	t.Helper()
	db := sql.OpenDB(quarantineTestConnector{s})
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	r, err := NewQuarantineRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func quarantineRepositorySource(t *testing.T, label string, valid bool) rabbitmq.ConsumerMessage {
	t.Helper()
	s := rabbitmq.ConsumerMessage{Body: []byte{0xff, 0, '{'}, ContentType: "application/json", DeliveryMode: 2, Exchange: rabbitmq.CommandsExchange, RoutingKey: rabbitmq.SubmitRoutingKey, MessageID: label, CorrelationID: "original-" + label,
		Headers: amqp.Table{"binary": []byte{0, 255}, "large": int64(9007199254740993), "nested": amqp.Table{"array": []any{true, amqp.Decimal{Scale: 2, Value: -12}}}}}
	if valid {
		command, err := messaging.Normalize(12, messaging.SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: messaging.Private, TargetID: "34", Media: messaging.Text, Content: label + "中🙂\x00"})
		if err != nil {
			t.Fatal(err)
		}
		wire, err := messaging.NewEnvelope(command, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		s.Body, err = json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		s.MessageID, s.CorrelationID = wire.CommandID, wire.PublishAttemptID
	}
	return s
}

func quarantineRepositoryInput(t *testing.T, source rabbitmq.ConsumerMessage, raw bool) rabbitmq.QuarantineInput {
	t.Helper()
	stamp := time.Date(2026, 10, 21, 1, 2, 3, 123456789, time.UTC)
	q, err := rabbitmq.NewQuarantineEnvelope(source, "DATA_CONSTRAINT", stamp.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	message := rabbitmq.ConsumerMessage{Body: q.Encode(), ContentType: rabbitmq.QuarantineContentType, Type: rabbitmq.QuarantineMessageType, DeliveryMode: 2, Exchange: rabbitmq.QuarantineExchange, RoutingKey: rabbitmq.QuarantineRoutingKey, MessageID: q.TransportID(), CorrelationID: "0123456789abcdef0123456789abcdef", Headers: amqp.Table{"trace": []byte{0, 255}}, ConsumerTag: "audit", DeliveryTag: 17}
	if raw {
		message, err = q.Source()
		if err != nil {
			t.Fatal(err)
		}
		message.Exchange, message.RoutingKey = rabbitmq.QuarantineExchange, rabbitmq.QuarantineRoutingKey
		if message.Headers == nil {
			message.Headers = amqp.Table{}
		}
		message.Headers["x-death"] = []any{amqp.Table{"queue": rabbitmq.PersistQueue, "exchange": rabbitmq.CommandsExchange, "routing-keys": []any{rabbitmq.SubmitRoutingKey}, "reason": "rejected", "count": int64(1), "time": stamp}}
		message.Headers["x-last-death-queue"], message.Headers["x-last-death-exchange"], message.Headers["x-last-death-reason"] = rabbitmq.PersistQueue, rabbitmq.CommandsExchange, "rejected"
		message.Expiration = ""
	}
	input, err := rabbitmq.ParseQuarantineInput(message, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func TestQuarantineRepositoryValidation(t *testing.T) {
	if _, err := NewQuarantineRepository(nil); !errors.Is(err, ErrQuarantineRepositoryUnavailable) {
		t.Fatal(err)
	}
	var absent *QuarantineRepository
	if _, err := absent.Save(context.Background(), rabbitmq.QuarantineInput{}); !errors.Is(err, ErrQuarantineRepositoryUnavailable) {
		t.Fatal(err)
	}
	s := &quarantineTestState{}
	r := newQuarantineTestRepository(t, s)
	if _, err := r.Save(context.Background(), rabbitmq.QuarantineInput{}); !errors.Is(err, rabbitmq.ErrQuarantineInput) {
		t.Fatal(err)
	}
	input := quarantineRepositoryInput(t, quarantineRepositorySource(t, "private source", true), false)
	if _, err := r.Save(nil, input); !errors.Is(err, rabbitmq.ErrQuarantineInput) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Save(ctx, input); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(s.events) != 0 {
		t.Fatal("invalid input accessed SQL")
	}
}

func TestQuarantineRepositoryTransactionBoundaries(t *testing.T) {
	input := quarantineRepositoryInput(t, quarantineRepositorySource(t, "private source", true), false)
	for _, stage := range []string{"", "begin", "insert", "read", "affected", "rowcount", "missing", "commit", "rollback"} {
		t.Run(stage, func(t *testing.T) {
			cause := errors.New("sensitive SQL source and credentials")
			s := &quarantineTestState{stage: stage, cause: cause}
			r := newQuarantineTestRepository(t, s)
			result, err := r.Save(context.Background(), input)
			if stage == "" {
				if err != nil || result.Disposition != QuarantineInserted || !reflect.DeepEqual(s.events, []string{"begin", "insert", "read", "commit"}) {
					t.Fatal(result, err, s.events)
				}
				return
			}
			if stage == "rollback" { // The duplicate path must wait for successful rollback.
				s.events = nil
				s.existing = true
				result, err = r.Save(context.Background(), input)
				if !errors.Is(err, ErrQuarantineRollbackUnknown) || !errors.Is(err, cause) {
					t.Fatal(err)
				}
			}
			if err == nil || result != (QuarantineAuditResult{}) {
				t.Fatal("uncertain/failed transaction returned success", result, err)
			}
			if stage == "commit" && !errors.Is(err, ErrQuarantineCommitUnknown) {
				t.Fatal(err)
			}
			if stage == "rowcount" && !errors.Is(err, ErrQuarantineInvariant) {
				t.Fatal(err)
			}
			if stage != "rowcount" && stage != "missing" && !errors.Is(err, cause) {
				t.Fatal("lost cause", err)
			}
			for _, format := range []string{"%v", "%+v", "%#v"} {
				if strings.Contains(fmt.Sprintf(format, err), "sensitive") {
					t.Fatal("error leaked source")
				}
			}
		})
	}
}

func TestQuarantineRepositoryDuplicateAndIntegrity(t *testing.T) {
	source := quarantineRepositorySource(t, "retained first observation", true)
	first := quarantineRepositoryInput(t, source, false)
	s := &quarantineTestState{}
	r := newQuarantineTestRepository(t, s)
	if _, err := r.Save(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	s.existing = true
	original := append([]driver.Value(nil), s.row...)
	second := quarantineRepositoryInput(t, source, true)
	for _, state := range []string{"open", "replay_pending", "resolved"} {
		s.row = append([]driver.Value(nil), original...)
		s.row[9] = state
		if state != "open" {
			s.row[10] = "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1"
		}
		if state == "resolved" {
			s.row[12] = "2026-10-21 02:03:04.000000"
		}
		before := append([]driver.Value(nil), s.row...)
		s.events = nil
		got, err := r.Save(context.Background(), second)
		if err != nil || got.Disposition != QuarantineExisting || !reflect.DeepEqual(before, s.row) || !reflect.DeepEqual(s.events, []string{"begin", "insert", "read", "rollback"}) {
			t.Fatal("duplicate replaced canonical audit", got, err, s.events)
		}
	}
	for name, mutate := range map[string]func([]driver.Value){
		"body":     func(v []driver.Value) { v[5] = []byte("changed") },
		"document": func(v []driver.Value) { v[6] = []byte(`{"audit_version":2}`) },
		"identity": func(v []driver.Value) { v[1] = "999" },
		"code":     func(v []driver.Value) { v[7] = "OTHER" },
		"detail":   func(v []driver.Value) { v[8] = "old raw headers" },
		"created":  func(v []driver.Value) { v[11] = "2026-10-21 00:00:00.000000" },
		"state":    func(v []driver.Value) { v[9] = "unknown" },
		"resolved": func(v []driver.Value) { v[12] = "2026-10-21 02:00:00.000000" },
		"replay":   func(v []driver.Value) { v[10] = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			s.row = append([]driver.Value(nil), original...)
			mutate(s.row)
			got, err := r.Save(context.Background(), second)
			if !errors.Is(err, ErrQuarantineInvariant) || got != (QuarantineAuditResult{}) {
				t.Fatal(got, err)
			}
		})
	}
	// A valid stored snapshot under the wrong transport key models key/content
	// disagreement without pretending to manufacture a SHA-256 collision.
	other := quarantineRepositoryInput(t, quarantineRepositorySource(t, "different", false), false)
	otherState := &quarantineTestState{}
	otherRepo := newQuarantineTestRepository(t, otherState)
	if _, err := otherRepo.Save(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	s.row = append([]driver.Value(nil), otherState.row...)
	s.row[0] = first.Envelope().TransportID()
	if got, err := r.Save(context.Background(), first); !errors.Is(err, ErrQuarantineConflict) || got != (QuarantineAuditResult{}) {
		t.Fatal(got, err)
	}
	s.stage = "missing"
	if got, err := r.Save(context.Background(), first); !errors.Is(err, sql.ErrNoRows) || got != (QuarantineAuditResult{}) {
		t.Fatal("duplicate on unrelated key accepted without requested audit", got, err)
	}
}
