package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func topologySupervisorOptions() SupervisorOptions {
	o := testSupervisorOptions()
	o.TopologyCheckInterval = 20 * time.Millisecond
	o.Session.OperationTimeout = 100 * time.Millisecond
	o.Session.CloseTimeout = time.Second
	return o
}

func awaitTopologyEvent(t *testing.T, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(2 * time.Second):
		t.Fatal("topology lifecycle event did not complete")
	}
}

func TestSupervisorPeriodicTopologySuccessKeepsEpochAndAdmission(t *testing.T) {
	p := newSupervisorPeer()
	checks := make(chan struct{}, 4)
	var active, maximum atomic.Int32
	p.onVerify = func(ctx context.Context) error {
		if n := active.Add(1); n > maximum.Load() {
			maximum.Store(n)
		}
		defer active.Add(-1)
		select {
		case checks <- struct{}{}:
		default:
		}
		return nil
	}
	s := startSupervisor(context.Background(), topologySupervisorOptions(), func(context.Context, uint64) (supervisedSession, error) { return p, nil })
	t.Cleanup(func() { closeTestSupervisor(t, s) })
	epoch := readySupervisor(t, s)
	for i := 0; i < 3; i++ {
		awaitTopologyEvent(t, checks)
		status := s.Status()
		if !status.Ready || status.ConnectionEpoch != epoch || status.LastError != nil {
			t.Fatal("successful check changed healthy epoch", status)
		}
	}
	if p.publications.Load() != 0 || maximum.Load() != 1 {
		t.Fatal("checks published or overlapped")
	}
	if result, err := s.Publish(context.Background(), publisherCommand(t), time.Now()); err != nil || result.Outcome != PublishAccepted {
		t.Fatal("healthy checks prevented admission", result, err)
	}
}

func TestSupervisorPeriodicTopologyFailureRevokesIdleEpochAndWaitsForCleanup(t *testing.T) {
	old, fresh := newSupervisorPeer(), newSupervisorPeer()
	old.idle = make(chan struct{})
	disposed := make(chan struct{})
	old.onClose = func() { close(disposed) }
	old.onVerify = func(context.Context) error { return errors.New("PRIVATE_TOPOLOGY_CAUSE") }
	var attempts atomic.Int32
	retried := make(chan struct{}, 8)
	repaired := make(chan struct{})
	s := startSupervisor(context.Background(), topologySupervisorOptions(), func(ctx context.Context, epoch uint64) (supervisedSession, error) {
		if attempts.Add(1) == 1 {
			return old, nil
		}
		select {
		case retried <- struct{}{}:
		default:
		}
		select {
		case <-repaired:
			return fresh, nil
		default:
			return nil, ErrSupervisorTopology
		}
	})
	// Always release the artificial application-call barrier, including failures.
	t.Cleanup(func() {
		select {
		case <-old.idle:
		default:
			close(old.idle)
		}
		closeTestSupervisor(t, s)
	})
	initial := readySupervisor(t, s)
	awaitTopologyEvent(t, disposed)
	status := s.Status()
	if status.Ready || !errors.Is(status.LastError, ErrSupervisorTopology) || strings.Contains(fmt.Sprintf("%+v", status), "PRIVATE") {
		t.Fatal("drift stayed ready or exposed its cause", status)
	}
	if attempts.Load() != 1 || old.publications.Load() != 0 {
		t.Fatal("idle drift needed a publication or overlapped old cleanup")
	}
	if result, err := s.Publish(context.Background(), publisherCommand(t), time.Now()); !errors.Is(err, ErrPublisherUnavailable) || result.Outcome != PublishNotStarted {
		t.Fatal("unready epoch admitted a command", result, err)
	}
	close(old.idle)
	awaitTopologyEvent(t, retried)
	observer, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.WaitReady(observer); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unrepaired topology became ready", err)
	}
	close(repaired)
	if epoch := readySupervisor(t, s); epoch <= initial {
		t.Fatal("repair reused an old epoch", epoch)
	}
	if old.publications.Load() != 0 || fresh.publications.Load() != 0 {
		t.Fatal("recovery replayed a command")
	}
}

func TestSupervisorPeriodicTopologyDeadlineRetiresEvenLateNil(t *testing.T) {
	p := newSupervisorPeer()
	p.idle = make(chan struct{})
	entered, disposed := make(chan struct{}), make(chan struct{})
	p.onVerify = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return nil // A verifier cannot turn an expired check into success.
	}
	p.onClose = func() { close(disposed) }
	var attempts atomic.Int32
	s := startSupervisor(context.Background(), topologySupervisorOptions(), func(ctx context.Context, epoch uint64) (supervisedSession, error) {
		if attempts.Add(1) == 1 {
			return p, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	t.Cleanup(func() { close(p.idle); closeTestSupervisor(t, s) })
	readySupervisor(t, s)
	awaitTopologyEvent(t, entered)
	awaitTopologyEvent(t, disposed)
	if s.Status().Ready || !errors.Is(s.Status().LastError, ErrSupervisorTopology) {
		t.Fatal("expired check kept readiness", s.Status())
	}
}

func TestSupervisorPeriodicTopologyCloseJoinsCheckAndCannotRevive(t *testing.T) {
	p := newSupervisorPeer()
	entered, returned := make(chan struct{}), make(chan struct{})
	p.onVerify = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(returned)
		return nil
	}
	var attempts atomic.Int32
	s := startSupervisor(context.Background(), topologySupervisorOptions(), func(context.Context, uint64) (supervisedSession, error) { attempts.Add(1); return p, nil })
	t.Cleanup(func() { closeTestSupervisor(t, s) })
	readySupervisor(t, s)
	awaitTopologyEvent(t, entered)
	closeTestSupervisor(t, s)
	awaitTopologyEvent(t, returned)
	if status := s.Status(); status.Ready || status.State != SupervisorClosed || attempts.Load() != 1 {
		t.Fatal("late verifier success revived closed supervisor", status)
	}
}

func TestSupervisorPeriodicTopologyLateSuccessCannotReviveRetiredSession(t *testing.T) {
	old, fresh := newSupervisorPeer(), newSupervisorPeer()
	entered, release := make(chan struct{}), make(chan struct{})
	old.onVerify = func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}
	var attempts atomic.Int32
	s := startSupervisor(context.Background(), topologySupervisorOptions(), func(context.Context, uint64) (supervisedSession, error) {
		if attempts.Add(1) == 1 {
			return old, nil
		}
		return fresh, nil
	})
	t.Cleanup(func() { closeTestSupervisor(t, s) })
	initial := readySupervisor(t, s)
	awaitTopologyEvent(t, entered)
	old.abort()
	if s.Status().Ready || attempts.Load() != 1 {
		t.Fatal("old verification overlapped a replacement")
	}
	close(release)
	if epoch := readySupervisor(t, s); epoch <= initial || !s.Status().Ready {
		t.Fatal("late success prevented new verified epoch", epoch)
	}
}

func TestSupervisorTopologyIntervalValidation(t *testing.T) {
	for _, interval := range []time.Duration{-time.Second, 0, time.Millisecond, time.Minute + 1} {
		o := testSupervisorOptions()
		o.TopologyCheckInterval = interval
		if err := ValidateSupervisorOptions(testSessionURI, LocalVHost, o); !errors.Is(err, ErrSupervisorConfig) {
			t.Fatal("unbounded or disabled checking accepted", interval, err)
		}
	}
	for _, interval := range []time.Duration{10 * time.Millisecond, time.Minute} {
		o := testSupervisorOptions()
		o.TopologyCheckInterval = interval
		if err := ValidateSupervisorOptions(testSessionURI, LocalVHost, o); err != nil {
			t.Fatal("valid checking interval rejected", interval, err)
		}
	}
}
