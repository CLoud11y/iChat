package health

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestStorageStopObserverKeepsProbeOwned(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	s := StartStorage(func(ctx context.Context) error { close(entered); <-ctx.Done(); <-release; return ctx.Err() }, time.Second, time.Second)
	defer s.Stop()
	defer finish()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe not started")
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.StopContext(expired); !errors.Is(err, context.Canceled) || s.StorageReady() {
		t.Fatal("expired observer did not stop readiness", err)
	}
	select {
	case <-s.Done():
		t.Fatal("probe was abandoned")
	default:
	}
	finish()
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := s.StopContext(ctx); err != nil {
		t.Fatal(err)
	}
	if s.StopContext(nil) == nil {
		t.Fatal("nil observer accepted")
	}
}
