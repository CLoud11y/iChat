package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"iChat/messaging"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type pageTestState struct {
	membership   bool
	conversation []driver.Value
	messages     [][]driver.Value
	fail         string
	cause        error
	queries      []string
	args         [][]driver.NamedValue
	rolledBack   bool
}
type pageTestConnector struct{ state *pageTestState }
type pageTestConn struct{ state *pageTestState }
type pageTestTx struct{ state *pageTestState }
type pageTestRows struct {
	columns int
	rows    [][]driver.Value
	state   *pageTestState
	stage   string
}

func (c pageTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &pageTestConn{c.state}, nil
}
func (pageTestConnector) Driver() driver.Driver { return commandTestDriver{} }
func (*pageTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare/write")
}
func (*pageTestConn) Close() error              { return nil }
func (*pageTestConn) Begin() (driver.Tx, error) { return nil, errors.New("explicit snapshot required") }
func (*pageTestConn) CheckNamedValue(value *driver.NamedValue) error {
	if n, ok := value.Value.(uint64); ok && n > math.MaxInt64 {
		return nil // Match MySQL's unsigned BIGINT parameter support.
	}
	return driver.ErrSkip
}
func (c *pageTestConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) || !opts.ReadOnly {
		return nil, errors.New("unsafe snapshot")
	}
	if c.state.fail == "begin" {
		return nil, c.state.cause
	}
	return &pageTestTx{c.state}, nil
}
func (*pageTestTx) Commit() error { return errors.New("read must not commit") }
func (tx *pageTestTx) Rollback() error {
	tx.state.rolledBack = true
	if tx.state.fail == "rollback" {
		return tx.state.cause
	}
	return nil
}
func (c *pageTestConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "FOR ") || strings.Contains(query, "SKIP LOCKED") {
		return nil, errors.New("locking read")
	}
	c.state.queries = append(c.state.queries, query)
	c.state.args = append(c.state.args, args)
	rows := &pageTestRows{state: c.state}
	switch query {
	case selectReadMembershipSQL:
		rows.columns, rows.stage, rows.rows = 1, "membership", [][]driver.Value{{c.state.membership}}
	case selectReadConversationSQL:
		rows.columns, rows.stage = 5, "conversation"
		if c.state.conversation != nil {
			rows.rows = [][]driver.Value{c.state.conversation}
		}
	case selectSyncMessagesSQL, selectHistoryMessagesSQL:
		rows.columns, rows.stage, rows.rows = 11, "messages", c.state.messages
	default:
		return nil, fmt.Errorf("unexpected query %s", query)
	}
	if c.state.fail == rows.stage {
		return nil, c.state.cause
	}
	return rows, nil
}
func (r *pageTestRows) Columns() []string { return make([]string, r.columns) }
func (r *pageTestRows) Close() error {
	if r.stage == "messages" && r.state.fail == "close" {
		return r.state.cause
	}
	return nil
}
func (r *pageTestRows) Next(values []driver.Value) error {
	if len(r.rows) == 0 {
		if r.stage == "messages" && r.state.fail == "iterate" {
			return r.state.cause
		}
		return io.EOF
	}
	copy(values, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}
func pageTestRepository(t *testing.T, state *pageTestState) *MessageRepository {
	t.Helper()
	pool := sql.OpenDB(pageTestConnector{state})
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	repo, err := NewMessageRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}
func pageTestMessage(t *testing.T, seq uint64, kind messaging.TargetType) []driver.Value {
	t.Helper()
	command, err := messaging.Normalize(1, messaging.SendRequest{ClientMessageID: fmt.Sprintf("00000000-0000-4000-8000-%012x", seq&0xffffffffffff), TargetType: kind, TargetID: "2", Media: messaging.Audio, Content: "  中🙂\x00e\u0301  "})
	if err != nil {
		t.Fatal(err)
	}
	return []driver.Value{command.CommandID, command.ClientMessageID, int64(1), int64(2), string(kind), string(command.Media), command.Content, command.ConversationID, strconv.FormatUint(seq, 10), "2026-10-12 00:00:00.123456", command.PayloadHash}
}
func pageTestPrivate(last uint64) *pageTestState {
	return &pageTestState{conversation: []driver.Value{"private", int64(1), int64(2), nil, strconv.FormatUint(last, 10)}}
}

func TestReliableMessagePagesAndCursorBoundaries(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(fmt.Sprint("history=", history), func(t *testing.T) {
			state := pageTestPrivate(4)
			state.messages = [][]driver.Value{pageTestMessage(t, 1, messaging.Private), pageTestMessage(t, 2, messaging.Private), pageTestMessage(t, 3, messaging.Private)}
			repo := pageTestRepository(t, state)
			if history {
				state.messages = [][]driver.Value{pageTestMessage(t, 4, messaging.Private), pageTestMessage(t, 3, messaging.Private), pageTestMessage(t, 2, messaging.Private)}
				page, err := repo.HistoryMessages(context.Background(), 2, "p:1:2", "", 2)
				if err != nil || page.BeforeSeq != nil || page.NextBeforeSeq != 3 || !page.HasMore || page.HighWatermark != 4 || len(page.Items) != 2 || page.Items[0].Seq != 3 || page.Items[1].Seq != 4 {
					t.Fatalf("page=%+v %v", page, err)
				}
			} else {
				page, err := repo.SyncMessages(context.Background(), 2, "p:1:2", "0", 2)
				if err != nil || page.NextAfterSeq != 2 || !page.HasMore || page.HighWatermark != 4 || len(page.Items) != 2 || page.Items[0].Seq != 1 || page.Items[0].Media != messaging.Audio || page.Items[0].ServerCreatedAt != "2026-10-12T00:00:00.123456Z" {
					t.Fatalf("page=%+v %v", page, err)
				}
			}
			if !state.rolledBack || len(state.queries) != 2 || state.args[1][len(state.args[1])-1].Value != int64(3) {
				t.Fatalf("unbounded/unsafe read %+v", state)
			}
		})
	}
	state := pageTestPrivate(math.MaxUint64)
	state.messages = [][]driver.Value{pageTestMessage(t, math.MaxUint64, messaging.Private)}
	repo := pageTestRepository(t, state)
	page, err := repo.SyncMessages(context.Background(), 1, "p:1:2", strconv.FormatUint(math.MaxUint64-1, 10), 1)
	if err != nil || page.NextAfterSeq != messaging.Sequence(math.MaxUint64) || page.HasMore {
		t.Fatalf("uint64 sync=%+v %v", page, err)
	}
	// Latest history must include MaxUint64 without computing watermark+1.
	state.messages = [][]driver.Value{pageTestMessage(t, math.MaxUint64, messaging.Private), pageTestMessage(t, math.MaxUint64-1, messaging.Private)}
	history, err := repo.HistoryMessages(context.Background(), 1, "p:1:2", "", 1)
	if err != nil || history.NextBeforeSeq != messaging.Sequence(math.MaxUint64) || !history.HasMore {
		t.Fatalf("uint64 history=%+v %v", history, err)
	}
}

func TestReliableMessagePagesEmptyAndAuthorization(t *testing.T) {
	for _, last := range []uint64{0, 4} {
		state := pageTestPrivate(last)
		repo := pageTestRepository(t, state)
		page, err := repo.SyncMessages(context.Background(), 1, "p:1:2", fmt.Sprint(last), 0)
		if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextAfterSeq != messaging.Sequence(last) || page.HasMore || len(state.queries) != 1 {
			t.Fatalf("empty=%+v %v", page, err)
		}
		history, err := repo.HistoryMessages(context.Background(), 1, "p:1:2", "0", 1)
		if err != nil || history.BeforeSeq == nil || *history.BeforeSeq != 0 || history.NextBeforeSeq != 0 || history.Items == nil || history.HasMore {
			t.Fatalf("before0=%+v %v", history, err)
		}
	}
	state := &pageTestState{}
	repo := pageTestRepository(t, state)
	page, err := repo.SyncMessages(context.Background(), 1, "p:1:2", "", 1)
	if err != nil || page.HighWatermark != 0 || page.Items == nil {
		t.Fatalf("absent authorized=%+v %v", page, err)
	}
	if _, err := repo.SyncMessages(context.Background(), 3, "p:1:2", "0", 1); !errors.Is(err, ErrConversationForbidden) {
		t.Fatalf("cross-user %v", err)
	}
	if len(state.queries) != 1 {
		t.Fatal("cross-user queried storage")
	}
	if _, err := repo.SyncMessages(context.Background(), 1, "p:1:2", "1", 1); !errors.Is(err, messaging.ErrInvalidInput) {
		t.Fatalf("future cursor %v", err)
	}
	for _, member := range []bool{false, true} {
		state := &pageTestState{membership: member}
		repo := pageTestRepository(t, state)
		page, err := repo.SyncMessages(context.Background(), 1, "g:2", "0", 1)
		if member && (err != nil || page.Items == nil) || !member && !errors.Is(err, ErrConversationForbidden) {
			t.Fatalf("membership=%t %+v %v", member, page, err)
		}
		if state.queries[0] != selectReadMembershipSQL || state.args[0][0].Value != int64(2) || state.args[0][1].Value != int64(1) {
			t.Fatalf("wrong authorization args %v", state.args)
		}
	}
}

func TestReliableMessagePagesFailClosed(t *testing.T) {
	cause := errors.New("storage unavailable")
	for _, stage := range []string{"begin", "membership", "conversation", "messages", "iterate", "close", "rollback"} {
		t.Run(stage, func(t *testing.T) {
			state := &pageTestState{membership: true, conversation: []driver.Value{"group", nil, nil, int64(2), int64(1)}, messages: [][]driver.Value{pageTestMessage(t, 1, messaging.Group)}, fail: stage, cause: cause}
			repo := pageTestRepository(t, state)
			page, err := repo.SyncMessages(context.Background(), 1, "g:2", "0", 1)
			if !errors.Is(err, cause) || !reflect.DeepEqual(page, messaging.SyncPage{}) {
				t.Fatalf("partial success %+v %v", page, err)
			}
			history, err := repo.HistoryMessages(context.Background(), 1, "g:2", "", 1)
			if !errors.Is(err, cause) || !reflect.DeepEqual(history, messaging.HistoryPage{}) {
				t.Fatalf("partial history %+v %v", history, err)
			}
		})
	}
	for _, index := range []int{0, 1, 4, 5, 6, 7, 8, 9, 10} {
		t.Run(fmt.Sprint("corrupt-field-", index), func(t *testing.T) {
			state := pageTestPrivate(1)
			row := pageTestMessage(t, 1, messaging.Private)
			row[index] = "corrupt"
			state.messages = [][]driver.Value{row}
			page, err := pageTestRepository(t, state).SyncMessages(context.Background(), 1, "p:1:2", "0", 1)
			if err == nil || !reflect.DeepEqual(page, messaging.SyncPage{}) {
				t.Fatalf("corrupt page %+v %v", page, err)
			}
		})
	}
	for _, seqs := range [][]uint64{{}, {2}, {1}, {1, 3}, {1, 2, 4}} {
		state := pageTestPrivate(4)
		for _, seq := range seqs {
			state.messages = append(state.messages, pageTestMessage(t, seq, messaging.Private))
		}
		page, err := pageTestRepository(t, state).SyncMessages(context.Background(), 1, "p:1:2", "0", 2)
		if !errors.Is(err, ErrCommandInvariant) || !reflect.DeepEqual(page, messaging.SyncPage{}) {
			t.Fatalf("gap %v page=%+v %v", seqs, page, err)
		}
	}
	for _, seqs := range [][]uint64{{}, {3}, {4}, {4, 2}, {4, 3, 1}} {
		state := pageTestPrivate(4)
		for _, seq := range seqs {
			state.messages = append(state.messages, pageTestMessage(t, seq, messaging.Private))
		}
		page, err := pageTestRepository(t, state).HistoryMessages(context.Background(), 1, "p:1:2", "", 2)
		if !errors.Is(err, ErrCommandInvariant) || !reflect.DeepEqual(page, messaging.HistoryPage{}) {
			t.Fatalf("history gap %v page=%+v %v", seqs, page, err)
		}
	}
	state := pageTestPrivate(0)
	state.conversation[1] = int64(9)
	if _, err := pageTestRepository(t, state).SyncMessages(context.Background(), 1, "p:1:2", "0", 1); !errors.Is(err, ErrCommandInvariant) {
		t.Fatalf("bad conversation %v", err)
	}
}

func TestReliableMessagePagesValidateBeforeIO(t *testing.T) {
	state := pageTestPrivate(1)
	repo := pageTestRepository(t, state)
	for _, params := range [][3]string{{"0", "p:1:2", "0"}, {"1", "p:2:1", "0"}, {"1", "p:1:2", "01"}} {
		reader, _ := strconv.ParseUint(params[0], 10, 64)
		if _, err := repo.SyncMessages(context.Background(), reader, params[1], params[2], 1); !errors.Is(err, messaging.ErrInvalidInput) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.SyncMessages(ctx, 1, "p:1:2", "0", 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(state.queries) != 0 {
		t.Fatal("invalid request accessed storage")
	}
	if _, err := (*MessageRepository)(nil).SyncMessages(context.Background(), 1, "p:1:2", "0", 1); !errors.Is(err, ErrCommandRepositoryUnavailable) {
		t.Fatal(err)
	}
}
