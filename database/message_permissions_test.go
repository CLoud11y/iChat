package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"iChat/messaging"
	"io"
	"reflect"
	"strings"
	"testing"
)

type permissionTestState struct {
	active  map[string]bool
	failKey string
	cause   error
	reads   []string
}
type permissionTestConnector struct{ state *permissionTestState }
type permissionTestConn struct{ state *permissionTestState }
type permissionTestRows struct {
	columns []string
	values  []driver.Value
	done    bool
}

func (c permissionTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &permissionTestConn{state: c.state}, nil
}
func (permissionTestConnector) Driver() driver.Driver { return commandTestDriver{} }
func (*permissionTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*permissionTestConn) Close() error { return nil }
func (*permissionTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("permission rows only")
}
func (c *permissionTestConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	var table string
	switch {
	case strings.Contains(query, "FROM `user`"):
		table = "user"
	case strings.Contains(query, "FROM `group`"):
		table = "group"
	case strings.Contains(query, "FROM relation"):
		table = "relation"
	default:
		return nil, fmt.Errorf("unexpected permission query %s", query)
	}
	if !strings.Contains(query, "FOR SHARE") && !strings.Contains(query, "FOR UPDATE") {
		return nil, errors.New("permission read does not lock")
	}
	key := table
	keyArgs := args[:1]
	if table == "relation" {
		keyArgs = args[:3]
	}
	for _, arg := range keyArgs {
		key += fmt.Sprint(":", arg.Value)
	}
	c.state.reads = append(c.state.reads, key)
	if key == c.state.failKey {
		return nil, c.state.cause
	}
	columns := []string{"id", "active"}
	if strings.Contains(query, "phone") {
		columns = []string{"id", "phone"}
	}
	active, found := c.state.active[key]
	rows := &permissionTestRows{columns: columns}
	if found {
		rows.values = []driver.Value{args[0].Value, active}
		if columns[1] == "phone" {
			rows.values[1] = "test-phone"
		}
	}
	return rows, nil
}
func (r *permissionTestRows) Columns() []string { return r.columns }
func (*permissionTestRows) Close() error        { return nil }
func (r *permissionTestRows) Next(values []driver.Value) error {
	if r.done || r.values == nil {
		return io.EOF
	}
	r.done = true
	copy(values, r.values)
	return nil
}

func permissionTestDB(t *testing.T, state *permissionTestState) *sql.DB {
	t.Helper()
	db := sql.OpenDB(permissionTestConnector{state: state})
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func TestMessagePermissionDecisionsAndLockOrder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sender     uint64
		target     string
		targetType messaging.TargetType
		active     map[string]bool
		reason     string
		reads      []string
	}{
		{"private_reverse_ids", 10, "2", messaging.Private, map[string]bool{"user:2": true, "user:10": true, "relation:2:10:0": true, "relation:10:2:0": true}, "", []string{"user:2", "user:10", "relation:2:10:0", "relation:10:2:0"}},
		{"self", 2, "2", messaging.Private, nil, ReasonSelfSend, nil},
		{"sender_absent", 2, "10", messaging.Private, nil, ReasonSenderNotFound, []string{"user:2"}},
		{"target_deleted", 10, "2", messaging.Private, map[string]bool{"user:2": false}, ReasonTargetNotFound, []string{"user:2"}},
		{"one_way_friend", 2, "10", messaging.Private, map[string]bool{"user:2": true, "user:10": true, "relation:2:10:0": true}, ReasonNotFriends, []string{"user:2", "user:10", "relation:2:10:0", "relation:10:2:0"}},
		{"friend_deleted", 2, "10", messaging.Private, map[string]bool{"user:2": true, "user:10": true, "relation:2:10:0": false}, ReasonNotFriends, []string{"user:2", "user:10", "relation:2:10:0"}},
		{"group_allowed", 2, "10", messaging.Group, map[string]bool{"user:2": true, "group:10": true, "relation:2:10:1": true}, "", []string{"user:2", "group:10", "relation:2:10:1"}},
		{"group_absent", 2, "10", messaging.Group, map[string]bool{"user:2": true}, ReasonTargetNotFound, []string{"user:2", "group:10"}},
		{"group_deleted", 2, "10", messaging.Group, map[string]bool{"user:2": true, "group:10": false}, ReasonTargetNotFound, []string{"user:2", "group:10"}},
		{"membership_deleted", 2, "10", messaging.Group, map[string]bool{"user:2": true, "group:10": true, "relation:2:10:1": false}, ReasonNotGroupMember, []string{"user:2", "group:10", "relation:2:10:1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &permissionTestState{active: tc.active}
			command, err := messaging.Normalize(tc.sender, messaging.SendRequest{ClientMessageID: "00000000-0000-4000-8000-000000000001", TargetType: tc.targetType, TargetID: tc.target, Media: messaging.Text, Content: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			reason, err := authorizeCommand(context.Background(), permissionTestDB(t, state), command)
			if err != nil || reason != tc.reason || !reflect.DeepEqual(state.reads, tc.reads) {
				t.Fatalf("reason=%q err=%v reads=%v want=%v", reason, err, state.reads, tc.reads)
			}
		})
	}
}

func TestMessagePermissionSQLFailures(t *testing.T) {
	command := testCommand(t)
	for _, key := range []string{"user:1", "user:2", "relation:1:2:0", "relation:2:1:0"} {
		t.Run(key, func(t *testing.T) {
			state := &permissionTestState{active: map[string]bool{"user:1": true, "user:2": true, "relation:1:2:0": true, "relation:2:1:0": true}, failKey: key, cause: io.ErrUnexpectedEOF}
			reason, err := authorizeCommand(context.Background(), permissionTestDB(t, state), command)
			if reason != "" || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("SQL error became a business rejection: %q %v", reason, err)
			}
		})
	}
}
