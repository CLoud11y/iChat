package database

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

type quarantineQueryConnector struct{ state *quarantineQueryState }
type quarantineQueryState struct {
	rows       [][]driver.Value
	query      string
	args       []driver.NamedValue
	err        error
	iterateErr error
	closeErr   error
	writes     int
}
type quarantineQueryConn struct{ state *quarantineQueryState }
type quarantineQueryRows struct {
	state *quarantineQueryState
	index int
}

func (quarantineQueryConnector) Driver() driver.Driver { return nil }
func (c quarantineQueryConnector) Connect(context.Context) (driver.Conn, error) {
	return &quarantineQueryConn{c.state}, nil
}
func (c *quarantineQueryConn) Prepare(string) (driver.Stmt, error) {
	c.state.writes++
	return nil, errors.New("unexpected prepare")
}
func (c *quarantineQueryConn) Close() error { return nil }
func (c *quarantineQueryConn) Begin() (driver.Tx, error) {
	c.state.writes++
	return nil, errors.New("unexpected transaction")
}
func (c *quarantineQueryConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.state.query, c.state.args = query, args
	if c.state.err != nil {
		return nil, c.state.err
	}
	return &quarantineQueryRows{state: c.state}, nil
}
func (r *quarantineQueryRows) Columns() []string {
	n := 13
	if strings.Contains(r.state.query, "OCTET_LENGTH") {
		n = 12
	}
	return make([]string, n)
}
func (r *quarantineQueryRows) Close() error { return r.state.closeErr }
func (r *quarantineQueryRows) Next(dst []driver.Value) error {
	if r.index == len(r.state.rows) {
		if r.state.iterateErr != nil {
			return r.state.iterateErr
		}
		return io.EOF
	}
	copy(dst, r.state.rows[r.index])
	r.index++
	return nil
}

func newQuarantineQueryTestRepository(t *testing.T, state *quarantineQueryState) *QuarantineRepository {
	t.Helper()
	pool := sql.OpenDB(quarantineQueryConnector{state})
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
		if state.writes != 0 || strings.Contains(state.query, "FOR SHARE") || !strings.HasPrefix(state.query, "SELECT ") {
			t.Error("query used a write, lock or transaction")
		}
	})
	repo, _ := NewQuarantineRepository(pool)
	return repo
}

func quarantineQueryTestRow(t *testing.T, valid bool) []driver.Value {
	t.Helper()
	input := quarantineRepositoryInput(t, quarantineRepositorySource(t, "query-source", valid), false)
	doc, _ := input.EncodeAudit()
	source, _ := input.Envelope().Source()
	var sender, client, command, hash driver.Value
	if id, ok := input.Identity(); ok {
		sender, client, command, hash = id.SenderID, id.ClientMessageID, id.CommandID, id.PayloadHash
	}
	return []driver.Value{input.Envelope().TransportID(), sender, client, command, hash, source.Body, doc, "DATA_CONSTRAINT", quarantineAuditDetail, "open", nil, input.ReceivedAt().UTC().Truncate(time.Microsecond).Format(mysqlMicrosecondLayout), nil}
}

func TestQuarantineQueryGetIntegrity(t *testing.T) {
	ctx := context.Background()
	for _, valid := range []bool{false, true} {
		row := quarantineQueryTestRow(t, valid)
		state := &quarantineQueryState{rows: [][]driver.Value{row}}
		repo := newQuarantineQueryTestRepository(t, state)
		id := row[0].(string)
		detail, err := repo.Get(ctx, id)
		if err != nil || detail.Summary.TransportID != id || (detail.Summary.SenderID != nil) != valid {
			t.Fatal("valid detail rejected", err)
		}
		source, _ := detail.Input.Envelope().Source()
		if !bytes.Equal(source.Body, row[5].([]byte)) || len(state.args) != 1 || state.args[0].Value != id {
			t.Fatal("selection or raw content changed")
		}
		for _, mutate := range []func([]driver.Value){
			func(r []driver.Value) { r[5] = []byte("corrupt") },
			func(r []driver.Value) { r[6] = []byte(`{"audit_version":99}`) },
			func(r []driver.Value) { r[1] = "999" },
			func(r []driver.Value) { r[8] = "raw SQL secret" },
			func(r []driver.Value) { r[9] = "replay_pending" },
			func(r []driver.Value) { r[11] = "2026-10-22 00:00:00.000000" },
		} {
			bad := append([]driver.Value(nil), row...)
			mutate(bad)
			state.rows = [][]driver.Value{bad}
			got, err := repo.Get(ctx, id)
			if !errors.Is(err, ErrQuarantineInvariant) || got.Summary.TransportID != "" || strings.Contains(err.Error(), "secret") {
				t.Fatal("corrupt detail accepted or leaked", err)
			}
		}
		bad := append([]driver.Value(nil), row...)
		bad[0] = "mq:v1:" + strings.Repeat("0", 64)
		state.rows = [][]driver.Value{bad}
		if _, err := repo.Get(ctx, id); !errors.Is(err, ErrQuarantineConflict) {
			t.Fatal("mismatched key accepted", err)
		}
		state.rows = nil
		if _, err := repo.Get(ctx, id); !errors.Is(err, ErrQuarantineNotFound) {
			t.Fatal("missing detail fabricated", err)
		}
	}
}

func TestQuarantineQueryPaginationAndFailures(t *testing.T) {
	ctx := context.Background()
	first := "mq:v1:" + strings.Repeat("1", 64)
	second := "mq:v1:" + strings.Repeat("2", 64)
	metadata := func(id string) []driver.Value {
		return []driver.Value{id, nil, nil, nil, nil, "DATA_CONSTRAINT", "open", nil, "2026-10-22 01:02:03.000000", nil, int64(3), int64(800)}
	}
	state := &quarantineQueryState{rows: [][]driver.Value{metadata(first), metadata(second)}}
	repo := newQuarantineQueryTestRepository(t, state)
	page, err := repo.List(ctx, QuarantineListOptions{Limit: 1})
	if err != nil || len(page.Records) != 1 || page.Next != first || strings.Contains(state.query, "raw_body,") || len(state.args) != 3 || state.args[1].Value != "open" || state.args[2].Value != int64(2) {
		t.Fatal("bounded metadata page failed", err)
	}
	state.rows = [][]driver.Value{metadata(second)}
	page, err = repo.List(ctx, QuarantineListOptions{State: "all", After: first, Limit: 1})
	if err != nil || page.Next != "" || len(page.Records) != 1 || strings.Contains(state.query, "state=?") || state.args[0].Value != first {
		t.Fatal("all-state cursor failed", err)
	}
	state.rows = nil
	page, err = repo.List(ctx, QuarantineListOptions{Limit: 10})
	if err != nil || page.Records == nil || len(page.Records) != 0 {
		t.Fatal("empty page must encode as array", err)
	}
	secret := errors.New("SQL secret")
	for _, stage := range []string{"query", "iterate", "close", "scan"} {
		state.err, state.iterateErr, state.closeErr, state.rows = nil, nil, nil, nil
		switch stage {
		case "query":
			state.err = secret
		case "iterate":
			state.rows, state.iterateErr = [][]driver.Value{metadata(first)}, secret
		case "close":
			state.closeErr = secret
		case "scan":
			row := metadata(first)
			row[10] = "SQL secret"
			state.rows = [][]driver.Value{row}
		}
		page, err := repo.List(ctx, QuarantineListOptions{Limit: 10})
		if err == nil || strings.Contains(err.Error(), "secret") || !reflect.DeepEqual(page, QuarantinePage{}) {
			t.Fatal("failure returned a partial page or leaked SQL", stage, err)
		}
	}
}

func TestQuarantineQueryValidation(t *testing.T) {
	repo := &QuarantineRepository{}
	if _, err := repo.Get(context.Background(), ""); !errors.Is(err, ErrQuarantineRepositoryUnavailable) {
		t.Fatal(err)
	}
	for _, options := range []QuarantineListOptions{{Limit: 0}, {Limit: 101}, {Limit: 1, State: "secret"}, {Limit: 1, After: "secret"}} {
		if err := options.Validate(); !errors.Is(err, ErrQuarantineQuery) {
			t.Fatal("invalid query accepted")
		}
	}
	for _, id := range []string{"", "mq:v1:" + strings.Repeat("A", 64), "mq:v2:" + strings.Repeat("0", 64), "mq:v1:" + strings.Repeat("0", 63)} {
		if ValidQuarantineTransportID(id) {
			t.Fatal("invalid ID accepted")
		}
	}
	state := &quarantineQueryState{}
	pool := sql.OpenDB(quarantineQueryConnector{state})
	defer pool.Close()
	repo, _ = NewQuarantineRepository(pool)
	if _, err := repo.Get(nil, ""); !errors.Is(err, ErrQuarantineQuery) {
		t.Fatal(err)
	}
	if _, err := repo.Get(context.Background(), "secret"); !errors.Is(err, ErrQuarantineQuery) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.List(ctx, QuarantineListOptions{Limit: 1}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if state.query != "" {
		t.Fatal("invalid or canceled query reached SQL")
	}
}
