package clientsim

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"iChat/messaging"
)

const processReady = "ICHAT_CLIENTSIM_DISK_READY"

type diskProcess struct {
	cmd    *exec.Cmd
	done   chan struct{}
	err    error
	stderr bytes.Buffer
	input  io.WriteCloser
}

func startDiskProcess(t *testing.T, dir, mode string) *diskProcess {
	t.Helper()
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	p := &diskProcess{cmd: exec.CommandContext(ctx, program, "-test.run=^TestFileStoreProcessHelper$", "-test.timeout=18s"), done: make(chan struct{})}
	p.cmd.Env = append(os.Environ(), "ICHAT_CLIENTSIM_CHILD=1", "ICHAT_CLIENTSIM_DIR="+dir, "ICHAT_CLIENTSIM_MODE="+mode)
	p.cmd.Stderr = &p.stderr
	output, err := p.cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	p.input, err = p.cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		cancel()
		p.input.Close()
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		cancel()
		p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Error("file helper did not join")
		}
		p.input.Close()
	})
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(output).ReadString('\n')
		if err == nil && line != processReady+"\n" {
			err = errors.New("unexpected helper output")
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal("file helper failed to become ready", err)
		}
	case <-p.done:
		t.Fatal("file helper exited early", p.err, p.stderr.String())
	case <-time.After(8 * time.Second):
		t.Fatal("file helper readiness timeout")
	}
	return p
}

func (p *diskProcess) killAndJoin(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
		var exit *exec.ExitError
		if !errors.As(p.err, &exit) {
			t.Fatal("helper was not forcibly terminated", p.err, p.stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("killed file helper did not join")
	}
}

func afterProcessStore(t *testing.T, dir string) *FileStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		s, err := OpenFileStore(ctx, dir)
		if err == nil {
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			return s
		}
		if !errors.Is(err, ErrStoreBusy) {
			t.Fatal("failed reopening after process exit", err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("kernel lock not released", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestFileStoreCrossProcessOwnership(t *testing.T) {
	dir := t.TempDir()
	p := startDiskProcess(t, dir, "hold")
	lockBefore, err := os.Stat(filepath.Join(dir, lockName))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if s, err := OpenFileStore(context.Background(), dir); s != nil || !errors.Is(err, ErrStoreBusy) {
		t.Fatal("second process acquired active store", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("lock acquisition blocked")
	}
	p.killAndJoin(t)
	s := afterProcessStore(t, dir)
	lockAfter, err := os.Stat(filepath.Join(dir, lockName))
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("lock file was replaced or removed", err)
	}
	if body, err := s.Load(context.Background()); err != nil || body != nil {
		t.Fatal(err)
	}
}

func TestFileStoreCrossProcessCrashRecovery(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		state    SendState
		attempts messaging.Sequence
	}{
		{"queued", Queued, 0},
		{"submitting", PublishUnknown, 1},
		{"accepted", Accepted, 1},
		{"persisted", Persisted, 1},
		{"rejected", Rejected, 1},
		{"partial_write", Queued, 0},
		{"before_sync", PublishUnknown, 1},
		{"after_sync", PublishUnknown, 1},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			dir := t.TempDir()
			p := startDiskProcess(t, dir, tc.mode)
			before, err := os.Stat(filepath.Join(dir, journalName))
			if err != nil {
				t.Fatal(err)
			}
			p.killAndJoin(t)
			store := afterProcessStore(t, dir)
			sender := diskSender(t, store)
			got := simEntry(t, sender)
			request := simRequest(1)
			command, err := messaging.Normalize(12, request)
			if err != nil || got.Request != request || got.ClientMessageID != command.ClientMessageID || got.CommandID != command.CommandID || got.PayloadHash != command.PayloadHash || got.State != tc.state || got.Attempts != tc.attempts || got.InFlight {
				t.Fatal("process crash changed logical command", tc.mode, err)
			}
			if tc.state == Persisted || tc.state == Rejected {
				expected := simStatus(t, request, messaging.StatusState(tc.state))
				if !reflect.DeepEqual(got.Result, &expected) {
					t.Fatal("process crash changed terminal DTO")
				}
				if _, err := sender.BeginSubmit(context.Background(), command.ClientMessageID); !errors.Is(err, ErrBlocked) {
					t.Fatal(err)
				}
			} else {
				a := simBegin(t, sender, command.ClientMessageID)
				if a.Request != request || a.Number != tc.attempts+1 {
					t.Fatal("retry changed original request")
				}
			}
			if tc.mode == "partial_write" {
				// The torn candidate was removed before the recovery/new attempt
				// snapshots were appended. The recovered count above proves it
				// was not acknowledged or treated as a completed submission.
				if before.Size() <= int64(len(journalMagic)) {
					t.Fatal("no actual partial write")
				}
			}
		})
	}
}

// Only launched by the parent test binary. No defer/Close runs after Kill.
func TestFileStoreProcessHelper(t *testing.T) {
	if os.Getenv("ICHAT_CLIENTSIM_CHILD") != "1" {
		t.Skip("subprocess only")
	}
	store, err := OpenFileStore(context.Background(), os.Getenv("ICHAT_CLIENTSIM_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	mode := os.Getenv("ICHAT_CLIENTSIM_MODE")
	park := func() {
		if _, err := os.Stdout.Write([]byte(processReady + "\n")); err != nil {
			t.Fatal(err)
		}
		var one [1]byte
		os.Stdin.Read(one[:])
		t.Fatal("helper resumed without forced termination")
	}
	if mode == "hold" {
		park()
		return
	}
	sender := diskSender(t, store)
	entry := simQueue(t, sender, 1)
	if mode == "queued" {
		park()
		return
	}
	original := store.file
	switch mode {
	case "partial_write":
		store.file = &faultJournal{journalFile: original, write: func(b []byte, o int64) (int, error) {
			n, err := original.WriteAt(b[:len(b)/2], o)
			if err != nil {
				t.Fatal(err)
			}
			if err := original.Sync(); err != nil {
				t.Fatal(err)
			}
			park()
			return n, io.ErrShortWrite
		}}
	case "before_sync":
		store.file = &faultJournal{journalFile: original, syncFn: func() error { park(); return nil }}
	case "after_sync":
		store.file = &faultJournal{journalFile: original, syncFn: func() error {
			if err := original.Sync(); err != nil {
				t.Fatal(err)
			}
			park()
			return nil
		}}
	}
	a := simBegin(t, sender, entry.ClientMessageID)
	switch mode {
	case "submitting":
	case "accepted":
		if err := sender.CompleteSubmission(context.Background(), a, simSubmission(t, entry.Request, "accepted")); err != nil {
			t.Fatal(err)
		}
	case "persisted", "rejected":
		if err := sender.ApplyStatus(context.Background(), simStatus(t, entry.Request, messaging.StatusState(mode))); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unexpected helper mode")
	}
	park()
}
