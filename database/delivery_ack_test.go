package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"testing"

	"iChat/messaging"
)

type deliveryTestState struct {
	notificationTestState
	conversation                 []driver.Value
	stored                       uint64
	writes, commits              int
	boundUser, boundConversation any
}
type deliveryTestConnector struct{ s *deliveryTestState }
type deliveryTestConn struct {
	notificationTestConn
	s *deliveryTestState
}
type deliveryTestTx struct{ s *deliveryTestState }

func (c deliveryTestConnector) Driver() driver.Driver { return commandTestDriver{} }
func (c deliveryTestConnector) Connect(context.Context) (driver.Conn, error) {
	return deliveryTestConn{notificationTestConn{&c.s.notificationTestState}, c.s}, nil
}
func (c deliveryTestConn) CheckNamedValue(*driver.NamedValue) error { return nil }
func (c deliveryTestConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	if _, e := c.notificationTestConn.BeginTx(ctx, o); e != nil {
		return nil, e
	}
	return deliveryTestTx{c.s}, nil
}
func (t deliveryTestTx) Commit() error {
	t.s.commits++
	if t.s.fail == "commit" {
		return errors.New("PRIVATE")
	}
	return nil
}
func (t deliveryTestTx) Rollback() error {
	return notificationTestTx{&t.s.notificationTestState}.Rollback()
}
func (c deliveryTestConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	if q == selectReadConversationSQL+" FOR SHARE" {
		if c.s.fail == "conversation" {
			return nil, errors.New("PRIVATE")
		}
		return &statusTestRows{count: 5, values: c.s.conversation}, nil
	}
	if q == selectDeliverySQL {
		if c.s.fail == "cursor" {
			return nil, errors.New("PRIVATE")
		}
		return &statusTestRows{count: 1, values: []driver.Value{messaging.Sequence(c.s.stored).String()}}, nil
	}
	return c.notificationTestConn.QueryContext(ctx, q, a)
}
func (c deliveryTestConn) ExecContext(_ context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	if q != advanceDeliverySQL || len(a) != 3 {
		return nil, errors.New("unexpected ACK write")
	}
	c.s.writes++
	c.s.boundUser, c.s.boundConversation = a[0].Value, a[1].Value
	if c.s.fail == "write" {
		return nil, errors.New("PRIVATE")
	}
	return driver.RowsAffected(1), nil
}
func deliveryTestRepository(t *testing.T, s *deliveryTestState) *MessageRepository {
	t.Helper()
	pool := sql.OpenDB(deliveryTestConnector{s})
	t.Cleanup(func() { pool.Close() })
	r, e := NewMessageRepository(pool)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestDeliveryACKRepositoryReceiptsAndFailureGates(t *testing.T) {
	for _, mode := range []string{"ok", "lower", "zero", "max", "outsider", "inactive", "group_denied", "deleted_group", "overflow", "missing", "corrupt", "stored_ahead", "begin", "user", "group", "membership", "conversation", "write", "cursor", "commit", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			s := &deliveryTestState{notificationTestState: notificationTestState{active: true, member: true, group: true}, conversation: []driver.Value{"private", int64(1), int64(2), nil, "3"}, stored: 3}
			user, id, seq, want := uint64(1), "p:1:2", messaging.Sequence(3), messaging.Sequence(3)
			if mode == "group_denied" || mode == "deleted_group" || mode == "group" || mode == "membership" {
				id = "g:5"
				s.conversation = []driver.Value{"group", nil, nil, int64(5), "3"}
			}
			switch mode {
			case "lower":
				seq = 1
			case "zero":
				seq = 0
				s.stored = 0
				want = 0
			case "max":
				seq = messaging.Sequence(math.MaxUint64)
				s.stored = math.MaxUint64
				want = seq
				s.conversation[4] = "18446744073709551615"
			case "outsider":
				user = 3
			case "inactive":
				s.active = false
			case "group_denied":
				s.member = false
			case "deleted_group":
				s.group = false
			case "overflow":
				seq = 4
			case "missing":
				s.conversation = nil
			case "corrupt":
				s.conversation[1] = int64(3)
			case "stored_ahead":
				s.stored = 4
			case "rollback":
				s.stored = 4
				s.fail = mode
			case "ok":
			default:
				s.fail = mode
			}
			r := deliveryTestRepository(t, s)
			got, err := r.AcknowledgeDelivery(context.Background(), user, id, seq)
			success := mode == "ok" || mode == "lower" || mode == "zero" || mode == "max"
			if success {
				if err != nil || got != want || s.commits != 1 || s.writes != 1 || s.boundUser != user || s.boundConversation != id {
					t.Fatal(got, err, s)
				}
			} else {
				if err == nil || got != 0 {
					t.Fatal("false committed receipt", got, err)
				}
				if mode == "commit" && !errors.Is(err, ErrDeliveryCommitUnknown) {
					t.Fatal(err)
				}
				if mode != "commit" && mode != "rollback" && s.commits != 0 {
					t.Fatal("failed validation committed")
				}
			}
		})
	}
}
func TestDeliveryACKRepositoryInput(t *testing.T) {
	var r *MessageRepository
	for _, tc := range []struct {
		ctx  context.Context
		user uint64
		id   string
	}{{nil, 1, "p:1:2"}, {context.Background(), 0, "g:1"}, {context.Background(), 1, "g:01"}} {
		if _, e := r.AcknowledgeDelivery(tc.ctx, tc.user, tc.id, 0); !errors.Is(e, messaging.ErrInvalidInput) {
			t.Fatal(e)
		}
	}
	if _, e := r.AcknowledgeDelivery(context.Background(), 1, "p:1:2", 0); !errors.Is(e, ErrCommandRepositoryUnavailable) {
		t.Fatal(e)
	}
	s := &deliveryTestState{}
	repo := deliveryTestRepository(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := repo.AcknowledgeDelivery(ctx, 1, "p:1:2", 0); !errors.Is(e, context.Canceled) || len(s.events) != 0 {
		t.Fatal(e, s.events)
	}
}
