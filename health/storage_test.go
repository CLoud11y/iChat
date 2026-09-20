package health

import (
	"context"
	"errors"
	"testing"
	"time"
)

func awaitReady(t *testing.T, s *Storage, want bool) {
	t.Helper()
	deadline := time.After(time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if s.StorageReady() == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("storage readiness did not become %v", want)
		case <-tick.C:
		}
	}
}

func TestStorageRecoveryAndStop(t *testing.T) {
	entered := make(chan context.Context)
	results := make(chan error)
	s := StartStorage(func(ctx context.Context) error {
		select {
		case entered <- ctx:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case err := <-results:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}, time.Millisecond, time.Second)
	defer s.Stop()
	if s.StorageReady() {
		t.Fatal("ready before first probe")
	}
	next := func() context.Context {
		t.Helper()
		select {
		case ctx := <-entered:
			return ctx
		case <-time.After(time.Second):
			t.Fatal("probe did not run")
			return nil
		}
	}
	next()
	results <- errors.New("offline")
	next() // The failed probe has completed; a retry is running.
	if s.StorageReady() {
		t.Fatal("failed probe marked ready")
	}
	results <- nil
	awaitReady(t, s, true)
	next()
	results <- errors.New("lost connection")
	awaitReady(t, s, false)
	ctx := next()
	s.Stop()
	if ctx.Err() != context.Canceled || s.StorageReady() {
		t.Fatal("stop did not cancel probe and clear readiness")
	}
}

func TestStorageProbeTimeout(t *testing.T) {
	finished := make(chan error, 1)
	s := StartStorage(func(ctx context.Context) error {
		<-ctx.Done()
		finished <- ctx.Err()
		return nil // Even a late success cannot establish readiness.
	}, time.Hour, 10*time.Millisecond)
	defer s.Stop()
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("probe deadline missing")
	}
	s.Stop()
	if s.StorageReady() {
		t.Fatal("timed out probe marked ready")
	}
}

func TestStorageStaleAndMissing(t *testing.T) {
	var absent *Storage
	if absent.StorageReady() {
		t.Fatal("missing monitor ready")
	}
	s := &Storage{ready: true, checked: time.Now().Add(-time.Minute), maxAge: time.Second}
	if s.StorageReady() {
		t.Fatal("stale success ready")
	}
}
