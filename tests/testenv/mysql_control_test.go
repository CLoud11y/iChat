package testenv

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestMySQLControlOwnership(t *testing.T) {
	for _, running := range []bool{false, true} {
		for _, foreign := range []bool{false, true} {
			e := &Environment{runID: testRunID, owned: true, ready: true}
			e.conf.MYSQL.Host, e.conf.MYSQL.Port = "127.0.0.1", "12345"
			var mutations [][]string
			e.run = func(_ context.Context, args ...string) ([]byte, error) {
				switch args[0] {
				case "compose":
					if reflect.DeepEqual(args, e.composeArgs("port", "mysql", "3306")) {
						return []byte("127.0.0.1:12346"), nil
					}
					if !reflect.DeepEqual(args, e.composeArgs("ps", "--all", "--quiet", "mysql")) {
						t.Fatalf("unscoped lookup: %v", args)
					}
					return []byte("mysql-owned"), nil
				case "container":
					if !reflect.DeepEqual(args, []string{"container", "inspect", "mysql-owned"}) {
						t.Fatal(args)
					}
					labels := map[string]string{projectLabel: e.project(), runLabel: testRunID}
					if foreign {
						labels[runLabel] = "foreign"
					}
					return json.Marshal([]any{map[string]any{"Config": map[string]any{"Labels": labels}}})
				default:
					mutations = append(mutations, args)
					return nil, nil
				}
			}
			err := e.SetMySQLRunning(context.Background(), running)
			if foreign {
				if err == nil || len(mutations) != 0 {
					t.Fatal("foreign container modified")
				}
			} else {
				want := []string{"stop", "--time", "10", "mysql-owned"}
				if running {
					want = []string{"start", "mysql-owned"}
				}
				if err != nil || !reflect.DeepEqual(mutations, [][]string{want}) {
					t.Fatalf("mutations=%v err=%v", mutations, err)
				}
				if running && e.conf.MYSQL.Port != "12346" {
					t.Fatal("restarted endpoint was not refreshed")
				}
			}
		}
	}
}

func TestMySQLControlRejectsAmbiguousOrUnverifiableContainer(t *testing.T) {
	for _, tc := range []struct{ ids, inspection string }{
		{"", ""}, {"one two", ""}, {"one", "invalid-json"}, {"one", "[]"}, {"one", "[{}]"},
	} {
		e := &Environment{runID: testRunID, owned: true, ready: true}
		e.run = func(_ context.Context, args ...string) ([]byte, error) {
			switch args[0] {
			case "compose":
				return []byte(tc.ids), nil
			case "container":
				return []byte(tc.inspection), nil
			default:
				t.Fatalf("unverified mutation: %v", args)
				return nil, nil
			}
		}
		if err := e.SetMySQLRunning(context.Background(), false); err == nil {
			t.Fatal("accepted unverified container")
		}
	}
}

func TestMySQLControlRejectsUninitialized(t *testing.T) {
	for _, e := range []*Environment{{}, {runID: testRunID, owned: true}, {runID: testRunID, ready: true}, {runID: "bad", owned: true, ready: true}} {
		e.run = func(context.Context, ...string) ([]byte, error) { t.Fatal("unsafe Docker access"); return nil, nil }
		if err := e.SetMySQLRunning(context.Background(), false); err == nil {
			t.Fatal("uninitialized run accepted")
		}
	}
}
