package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"iChat/messaging"
)

type conversationsTestState struct {
	pageTestState
	active bool
	rows   [][]driver.Value
	reads  int
}
type conversationsTestConnector struct{ s *conversationsTestState }
type conversationsTestConn struct {
	*pageTestConn
	s *conversationsTestState
}

func (c conversationsTestConnector) Driver() driver.Driver { return commandTestDriver{} }
func (c conversationsTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &conversationsTestConn{&pageTestConn{&c.s.pageTestState}, c.s}, nil
}
func (c *conversationsTestConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.s.reads++
	c.s.queries = append(c.s.queries, q)
	c.s.args = append(c.s.args, args)
	r := &pageTestRows{state: &c.s.pageTestState, stage: "messages", columns: 7, rows: c.s.rows}
	if q == selectConversationReaderSQL {
		r.stage, r.columns, r.rows = "reader", 1, [][]driver.Value{{c.s.active}}
	} else if q != selectVisibleConversationsSQL {
		return nil, errors.New("unexpected conversation query")
	}
	if c.s.fail == r.stage {
		return nil, c.s.cause
	}
	return r, nil
}
func conversationsTestRepository(t *testing.T, s *conversationsTestState) *MessageRepository {
	t.Helper()
	s.cause = errors.New("PRIVATE_CONVERSATION_ERROR")
	pool := sql.OpenDB(conversationsTestConnector{s})
	t.Cleanup(func() { pool.Close() })
	r, e := NewMessageRepository(pool)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func conversationsPrivateRow(id string, low, high any, last, delivered string) []driver.Value {
	return []driver.Value{id, "private", low, high, nil, last, delivered}
}
func conversationsGroupRow(id string, group any, last, delivered string) []driver.Value {
	return []driver.Value{id, "group", nil, nil, group, last, delivered}
}

func TestConversationsPageOrderBoundariesAndLookahead(t *testing.T) {
	s := &conversationsTestState{active: true, rows: [][]driver.Value{conversationsGroupRow("g:10", int64(10), "9", "2"), conversationsGroupRow("g:2", int64(2), "1", "0"), conversationsPrivateRow("p:1:2", int64(1), int64(2), "3", "1")}}
	r := conversationsTestRepository(t, s)
	p, e := r.ListConversations(context.Background(), 1, "", 2)
	if e != nil || len(p.Items) != 2 || !p.HasMore || p.NextCursor != "g:2" || p.Items[0].DeliveredSeq != 2 {
		t.Fatal(p, e)
	}
	if !s.rolledBack || len(s.queries) != 2 || !strings.Contains(s.queries[1], "ORDER BY c.id ASC LIMIT ?") {
		t.Fatal("snapshot not released/bounded")
	}
	a := s.args[1]
	if len(a) != 7 || a[0].Value != int64(1) || a[1].Value != "" || a[2].Value != int64(1) || a[3].Value != int64(1) || a[4].Value != int64(1) || a[6].Value != int64(3) {
		t.Fatal(a)
	}
	for _, rowCount := range []int{0, 1, 2} {
		s := &conversationsTestState{active: true, rows: [][]driver.Value{conversationsPrivateRow("p:1:2", int64(1), int64(2), "0", "0"), conversationsPrivateRow("p:1:3", int64(1), int64(3), "1", "0")}[:rowCount]}
		r := conversationsTestRepository(t, s)
		p, e := r.ListConversations(context.Background(), 1, "g:999", 2)
		if e != nil || len(p.Items) != rowCount || p.HasMore || p.Items == nil {
			t.Fatal(p, e)
		}
		if rowCount == 0 && p.NextCursor != "g:999" {
			t.Fatal("empty cursor moved", p)
		}
	}
	max := &conversationsTestState{active: true, rows: [][]driver.Value{conversationsPrivateRow("p:1:18446744073709551615", int64(1), "18446744073709551615", "18446744073709551615", "18446744073709551615")}}
	p, e = conversationsTestRepository(t, max).ListConversations(context.Background(), math.MaxUint64, "", 1)
	if e != nil || p.Items[0].LastSeq != messaging.Sequence(math.MaxUint64) || p.Items[0].DeliveredSeq != p.Items[0].LastSeq {
		t.Fatal(p, e)
	}
}

func TestConversationsFailClosed(t *testing.T) {
	for _, mode := range []string{"begin", "reader", "inactive", "messages", "iterate", "close", "rollback", "scan", "identity", "kind", "wrong_owner", "wrong_pair", "group_shape", "wrong_group", "ahead", "duplicate", "reverse", "at_cursor", "lookahead_corrupt", "too_many"} {
		t.Run(mode, func(t *testing.T) {
			s := &conversationsTestState{active: true, rows: [][]driver.Value{conversationsPrivateRow("p:1:2", int64(1), int64(2), "3", "1")}}
			cursor := ""
			switch mode {
			case "inactive":
				s.active = false
			case "scan":
				s.rows[0][5] = "-1"
			case "identity":
				s.rows[0][0] = "p:01:2"
			case "kind":
				s.rows[0][1] = "group"
			case "wrong_owner":
				s.rows[0] = conversationsPrivateRow("p:2:3", int64(2), int64(3), "1", "0")
			case "wrong_pair":
				s.rows[0][3] = int64(3)
			case "group_shape":
				s.rows[0] = conversationsGroupRow("g:2", int64(2), "1", "0")
				s.rows[0][2] = int64(1)
			case "wrong_group":
				s.rows[0] = conversationsGroupRow("g:2", int64(3), "1", "0")
			case "ahead":
				s.rows[0][6] = "4"
			case "duplicate":
				s.rows = append(s.rows, append([]driver.Value(nil), s.rows[0]...))
			case "reverse":
				s.rows = append(s.rows, conversationsGroupRow("g:2", int64(2), "1", "0"))
			case "at_cursor":
				cursor = "p:1:2"
			case "lookahead_corrupt":
				s.rows = append(s.rows, conversationsPrivateRow("p:1:3", int64(1), int64(3), "1", "2"))
			case "too_many":
				s.rows = append(s.rows, conversationsPrivateRow("p:1:3", int64(1), int64(3), "1", "0"), conversationsPrivateRow("p:1:4", int64(1), int64(4), "1", "0"))
			default:
				s.fail = mode
			}
			r := conversationsTestRepository(t, s)
			p, e := r.ListConversations(context.Background(), 1, cursor, 1)
			if e == nil || !reflect.DeepEqual(p, messaging.ConversationPage{}) {
				t.Fatal("returned partial page", p, e)
			}
			if mode != "inactive" && mode != "scan" && mode != "identity" && mode != "kind" && mode != "wrong_owner" && mode != "wrong_pair" && mode != "group_shape" && mode != "wrong_group" && mode != "ahead" && mode != "duplicate" && mode != "reverse" && mode != "at_cursor" && mode != "lookahead_corrupt" && mode != "too_many" && !errors.Is(e, s.cause) {
				t.Fatal("lost SQL error cause", e)
			}
			if mode != "begin" && !s.rolledBack {
				t.Fatal("snapshot not released")
			}
		})
	}
}

func TestConversationsValidateBeforeIOAndCancellation(t *testing.T) {
	s := &conversationsTestState{active: true}
	r := conversationsTestRepository(t, s)
	for _, tc := range []struct {
		ctx    context.Context
		uid    uint64
		cursor string
		limit  int
	}{{nil, 1, "", 1}, {context.Background(), 0, "", 1}, {context.Background(), 1, "g:01", 1}, {context.Background(), 1, "", 501}} {
		if _, e := r.ListConversations(tc.ctx, tc.uid, tc.cursor, tc.limit); !errors.Is(e, messaging.ErrInvalidInput) {
			t.Fatal(e)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := r.ListConversations(ctx, 1, "", 1); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if s.reads != 0 {
		t.Fatal("invalid query touched SQL")
	}
	var missing *MessageRepository
	if _, e := missing.ListConversations(context.Background(), 1, "", 1); !errors.Is(e, ErrCommandRepositoryUnavailable) {
		t.Fatal(e)
	}
}
