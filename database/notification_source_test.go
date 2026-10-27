package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-redis/redis/v8"
)

type notificationChannelsState struct {
	active   bool
	ids      []int64
	fail     string
	released bool
}
type notificationChannelsConnector struct{ s *notificationChannelsState }
type notificationChannelsConn struct{ s *notificationChannelsState }
type notificationChannelsTx struct{ s *notificationChannelsState }

func (c notificationChannelsConnector) Driver() driver.Driver { return commandTestDriver{} }
func (c notificationChannelsConnector) Connect(context.Context) (driver.Conn, error) {
	return notificationChannelsConn{c.s}, nil
}
func (notificationChannelsConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected write")
}
func (notificationChannelsConn) Begin() (driver.Tx, error) {
	return nil, errors.New("explicit snapshot required")
}
func (notificationChannelsConn) Close() error { return nil }
func (c notificationChannelsConn) BeginTx(_ context.Context, o driver.TxOptions) (driver.Tx, error) {
	if !o.ReadOnly || o.Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) {
		return nil, errors.New("unsafe channel query")
	}
	return notificationChannelsTx{c.s}, nil
}
func (notificationChannelsTx) Commit() error { return errors.New("must not commit") }
func (t notificationChannelsTx) Rollback() error {
	t.s.released = true
	if t.s.fail == "release" {
		return errors.New("PRIVATE")
	}
	return nil
}
func (c notificationChannelsConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "SELECT EXISTS") {
		return &statusTestRows{count: 1, values: []driver.Value{c.s.active}}, nil
	}
	if !strings.Contains(q, "g.deleted_at IS NULL AND r.deleted_at IS NULL") || !strings.Contains(q, "ORDER BY g.id LIMIT ?") {
		return nil, errors.New("unsafe group scope")
	}
	if c.s.fail == "groups" {
		return nil, errors.New("PRIVATE")
	}
	rows := &pageTestRows{columns: 1, state: &pageTestState{}}
	for _, id := range c.s.ids {
		rows.rows = append(rows.rows, []driver.Value{id})
	}
	return rows, nil
}

func TestNotificationSourceBoundsAndMetadata(t *testing.T) {
	for _, mode := range []string{"empty", "groups", "disabled", "overflow", "groups_error", "release_error"} {
		t.Run(mode, func(t *testing.T) {
			s := &notificationChannelsState{active: true}
			if mode == "groups" {
				s.ids = []int64{3, 8}
			}
			if mode == "disabled" {
				s.active = false
			}
			if mode == "overflow" {
				s.ids = []int64{3, 8, 10}
			}
			if mode == "groups_error" {
				s.fail = "groups"
			}
			if mode == "release_error" {
				s.fail = "release"
			}
			pool := sql.OpenDB(notificationChannelsConnector{s})
			defer pool.Close()
			source := &NotificationSource{pool: pool}
			channels, e := source.Channels(context.Background(), 2, 2)
			if mode == "empty" || mode == "groups" {
				want := []string{"ichat:notify:v1:user:2"}
				if mode == "groups" {
					want = append(want, "ichat:notify:v1:group:3", "ichat:notify:v1:group:8")
				}
				if e != nil || !reflect.DeepEqual(channels, want) {
					t.Fatal(channels, e)
				}
			} else if e == nil || channels != nil {
				t.Fatal("failed metadata query returned channels", channels, e)
			}
			if !s.released {
				t.Fatal("metadata transaction not released")
			}
		})
	}
	var dials atomic.Int32
	client := redis.NewClient(&redis.Options{Dialer: func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("must not dial")
	}})
	defer client.Close()
	source := &NotificationSource{client: client}
	for _, channels := range [][]string{nil, {"private_2"}, {"2"}, {"ichat:notify:v1:user:02"}, {"ichat:notify:v1:group:0"}, {"ichat:notify:v1:user:2", "ichat:notify:v1:user:2"}} {
		if _, e := source.Subscribe(context.Background(), channels); e == nil {
			t.Fatal("invalid channel accepted")
		}
	}
	if dials.Load() != 0 {
		t.Fatal("invalid input reached Redis")
	}
}
