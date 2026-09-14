package testenv

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const testRunID = "0123456789abcdef0123456789abcdef"

func TestCloseChecksAllOwnershipBeforeRemoval(t *testing.T) {
	for _, foreign := range []string{"", "container", "volume", "network"} {
		t.Run("foreign_"+foreign, func(t *testing.T) {
			e := &Environment{runID: testRunID, owned: true}
			var removed []string
			e.run = func(_ context.Context, args ...string) ([]byte, error) {
				switch args[1] {
				case "ls":
					if !strings.Contains(strings.Join(args, " "), "label="+projectLabel+"="+e.project()) {
						t.Fatal("resource listing must be scoped to the run's project")
					}
					return []byte(args[0] + "_owned"), nil
				case "inspect":
					labels := map[string]string{projectLabel: e.project(), runLabel: testRunID}
					if args[0] == foreign {
						labels[runLabel] = "another-run"
					}
					record := map[string]any{"Labels": labels}
					if args[0] == "container" {
						record = map[string]any{"Config": record}
					}
					return json.Marshal([]any{record})
				case "rm":
					removed = append(removed, strings.Join(args, " "))
					return nil, nil
				default:
					t.Fatalf("unexpected docker call: %v", args)
					return nil, nil
				}
			}
			err := e.Close(context.Background())
			if foreign != "" {
				if err == nil || len(removed) != 0 {
					t.Fatalf("foreign resource: err=%v removals=%v", err, removed)
				}
				return
			}
			want := []string{"container rm --force container_owned", "volume rm volume_owned", "network rm network_owned"}
			if err != nil || !reflect.DeepEqual(removed, want) {
				t.Fatalf("Close: err=%v removals=%v, want %v", err, removed, want)
			}
			e.run = func(context.Context, ...string) ([]byte, error) {
				t.Fatal("second Close must not touch Docker resources")
				return nil, nil
			}
			if err := e.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStartCollisionDoesNotAdoptOrDeleteExistingProject(t *testing.T) {
	e := &Environment{runID: testRunID}
	e.run = func(_ context.Context, args ...string) ([]byte, error) {
		if args[1] != "ls" {
			t.Fatalf("must not modify an existing project: %v", args)
		}
		return []byte("existing-resource"), nil
	}
	if err := e.start(context.Background()); err == nil {
		t.Fatal("must reject reuse of existing resources")
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidRunIDCannotAccessDocker(t *testing.T) {
	for _, id := range []string{"", "iChat", "../dev", testRunID + "extra", strings.ToUpper(testRunID)} {
		e := &Environment{runID: id, owned: true}
		e.run = func(context.Context, ...string) ([]byte, error) {
			t.Fatal("invalid run ID reached Docker")
			return nil, nil
		}
		if err := e.start(context.Background()); err == nil {
			t.Fatalf("accepted run ID %q", id)
		}
		if err := e.Close(context.Background()); err == nil {
			t.Fatalf("cleanup accepted run ID %q", id)
		}
	}
}

func TestCloseInspectionFailureDoesNotRemoveAnything(t *testing.T) {
	for _, output := range []string{"invalid-json", "[]", "[{}]", "[{},{}]"} {
		e := &Environment{runID: testRunID, owned: true}
		e.run = func(_ context.Context, args ...string) ([]byte, error) {
			switch args[1] {
			case "ls":
				return []byte("some-resource"), nil
			case "inspect":
				return []byte(output), nil
			default:
				t.Fatalf("must not remove unverifiable resources: %v", args)
				return nil, nil
			}
		}
		if err := e.Close(context.Background()); err == nil {
			t.Fatalf("accepted inspect output %q", output)
		}
	}
	e := &Environment{runID: testRunID, owned: true}
	e.run = func(context.Context, ...string) ([]byte, error) { return nil, context.DeadlineExceeded }
	if err := e.Close(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup must report Docker failure: %v", err)
	}
}

func TestAddressRequiresSingleLoopbackPort(t *testing.T) {
	for _, address := range []string{"127.0.0.1:12345", "0.0.0.0:3306", "localhost:3306", "192.168.1.1:3306", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:abc", "127.0.0.1:1\n127.0.0.1:2"} {
		e := &Environment{run: func(context.Context, ...string) ([]byte, error) { return []byte(address), nil }}
		got, err := e.address(context.Background(), "mysql", "3306")
		if address == "127.0.0.1:12345" {
			if err != nil || got != address {
				t.Fatalf("valid endpoint: got=%q err=%v", got, err)
			}
		} else if err == nil {
			t.Fatalf("accepted unsafe endpoint %q", address)
		}
	}
}

func TestConfigUnavailableUntilReady(t *testing.T) {
	if _, err := (&Environment{}).Config(); err == nil {
		t.Fatal("uninitialized environment exposed config")
	}
}

func TestStartRejectsDevelopmentCompose(t *testing.T) {
	if _, err := Start(context.Background(), "docker-compose.yml"); err == nil || !strings.Contains(err.Error(), "requires docker-compose.test.yml") {
		t.Fatalf("must reject development compose before contacting Docker: %v", err)
	}
}

func TestFailedStartupStillOwnsPartialResources(t *testing.T) {
	e := &Environment{runID: testRunID}
	var upAttempted bool
	e.run = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "compose" {
			upAttempted = true
			return nil, errors.New("injected startup failure")
		}
		if args[1] == "ls" {
			return nil, nil
		}
		t.Fatalf("unexpected call: %v", args)
		return nil, nil
	}
	if err := e.start(context.Background()); err == nil || !upAttempted || !e.owned {
		t.Fatalf("partial startup must retain cleanup responsibility: err=%v owned=%v", err, e.owned)
	}
	if _, err := e.Config(); err == nil {
		t.Fatal("failed startup exposed config")
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
