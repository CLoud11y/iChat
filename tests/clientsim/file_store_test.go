package clientsim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func diskStore(t *testing.T, dir string) *FileStore {
	t.Helper()
	s, err := OpenFileStore(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func diskSender(t *testing.T, store *FileStore) *Sender {
	t.Helper()
	s, err := OpenSender(context.Background(), 12, store)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFileStoreRoundTripExclusiveOwnershipAndRetiredCallbacks(t *testing.T) {
	dir := t.TempDir()
	store := diskStore(t, dir)
	s := diskSender(t, store)
	e := simQueue(t, s, 1)
	a := simBegin(t, s, e.ClientMessageID)
	if other, err := OpenFileStore(context.Background(), filepath.Join(dir, ".")); other != nil || !errors.Is(err, ErrStoreBusy) {
		t.Fatal("second owner admitted", err)
	}
	first, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first[0] = '!'
	second, err := store.Load(context.Background())
	if err != nil || second[0] == '!' {
		t.Fatal("load returned owned bytes", err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", store, store, store), dir) {
		t.Fatal("store formatting exposed path")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); !errors.Is(err, ErrStoreClosed) {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), second); !errors.Is(err, ErrStoreClosed) {
		t.Fatal(err)
	}
	replacement := diskStore(t, dir)
	restored := diskSender(t, replacement)
	if got := simEntry(t, restored); got.State != PublishUnknown || got.Request != e.Request || got.Attempts != 1 || got.InFlight {
		t.Fatal("interrupted state not recovered")
	}
	before, _ := replacement.Load(context.Background())
	if err := s.CompleteUnknown(context.Background(), a); !errors.Is(err, ErrStoreUncertain) {
		t.Fatal("retired sender remained writable", err)
	}
	after, _ := replacement.Load(context.Background())
	if !bytes.Equal(before, after) {
		t.Fatal("old callback overwrote new owner")
	}
	if _, err := os.Stat(filepath.Join(dir, lockName)); err != nil {
		t.Fatal("persistent lock file removed", err)
	}
	if _, err := OpenSender(context.Background(), 34, replacement); !errors.Is(err, ErrSnapshot) {
		t.Fatal("cross-account file restore", err)
	}
}

type faultJournal struct {
	journalFile
	write    func([]byte, int64) (int, error)
	read     func([]byte, int64) (int, error)
	syncFn   func() error
	truncate func(int64) error
	stat     func() (os.FileInfo, error)
	closeFn  func() error
}

func (f *faultJournal) WriteAt(b []byte, o int64) (int, error) {
	if f.write != nil {
		return f.write(b, o)
	}
	return f.journalFile.WriteAt(b, o)
}
func (f *faultJournal) ReadAt(b []byte, o int64) (int, error) {
	if f.read != nil {
		return f.read(b, o)
	}
	return f.journalFile.ReadAt(b, o)
}
func (f *faultJournal) Sync() error {
	if f.syncFn != nil {
		return f.syncFn()
	}
	return f.journalFile.Sync()
}
func (f *faultJournal) Truncate(n int64) error {
	if f.truncate != nil {
		return f.truncate(n)
	}
	return f.journalFile.Truncate(n)
}
func (f *faultJournal) Stat() (os.FileInfo, error) {
	if f.stat != nil {
		return f.stat()
	}
	return f.journalFile.Stat()
}
func (f *faultJournal) Close() error {
	if f.closeFn != nil {
		return f.closeFn()
	}
	return f.journalFile.Close()
}

func TestFileStoreWriteFailuresRequireReopen(t *testing.T) {
	for _, mode := range []string{"before_write", "short_write", "written_error", "sync_error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			store := diskStore(t, dir)
			s := diskSender(t, store)
			e := simQueue(t, s, 1)
			original := store.file
			cause := errors.New("PRIVATE_DISK_FAILURE")
			wrapper := &faultJournal{journalFile: original}
			switch mode {
			case "before_write":
				wrapper.write = func([]byte, int64) (int, error) { return 0, cause }
			case "short_write":
				wrapper.write = func(b []byte, o int64) (int, error) { return original.WriteAt(b[:len(b)/2], o) }
			case "written_error":
				wrapper.write = func(b []byte, o int64) (int, error) {
					n, err := original.WriteAt(b, o)
					if err != nil {
						return n, err
					}
					return n, cause
				}
			case "sync_error":
				wrapper.syncFn = func() error {
					if err := original.Sync(); err != nil {
						return err
					}
					return cause
				}
			case "panic":
				wrapper.write = func(b []byte, o int64) (int, error) {
					if _, err := original.WriteAt(b, o); err != nil {
						t.Fatal(err)
					}
					panic("PRIVATE_STORAGE_PANIC")
				}
			}
			store.file = wrapper
			if mode == "panic" {
				func() {
					defer func() {
						if recover() == nil {
							t.Error("expected callback panic")
						}
					}()
					s.BeginSubmit(context.Background(), e.ClientMessageID)
				}()
			} else {
				a, err := s.BeginSubmit(context.Background(), e.ClientMessageID)
				if !errors.Is(err, ErrStoreUncertain) || a.Request.Content != "" || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "PRIVATE") {
					t.Fatal("invalid uncertain outcome", err)
				}
				if mode == "short_write" {
					if !errors.Is(err, io.ErrShortWrite) {
						t.Fatal(err)
					}
				} else if !errors.Is(err, cause) {
					t.Fatal(err)
				}
			}
			if _, err := store.Load(context.Background()); !errors.Is(err, ErrStoreUncertain) {
				t.Fatal("faulted disk remained usable", err)
			}
			if err := store.Save(context.Background(), []byte("unsafe")); !errors.Is(err, ErrStoreUncertain) {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := diskSender(t, diskStore(t, dir))
			got := simEntry(t, reopened)
			want := PublishUnknown
			if mode == "before_write" || mode == "short_write" {
				want = Queued
			}
			if got.State != want || got.Request != e.Request || uint64(got.Attempts) != map[bool]uint64{true: 0, false: 1}[want == Queued] {
				t.Fatal("wrong recovered write outcome", mode)
			}
		})
	}
}

func TestFileStoreTornTailAndCompleteCorruption(t *testing.T) {
	base := append([]byte(journalMagic), encodeJournalFrame(1, []byte("first snapshot"))...)
	next := encodeJournalFrame(2, []byte("next snapshot"))
	for _, cut := range []int{1, 4, 7, 8, 15, 16, 31, frameHeader - 1, frameHeader, frameHeader + 3, len(next) - 1} {
		t.Run(fmt.Sprint("torn_", cut), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, journalName)
			if err := os.WriteFile(path, append(append([]byte(nil), base...), next[:cut]...), 0600); err != nil {
				t.Fatal(err)
			}
			store := diskStore(t, dir)
			body, err := store.Load(context.Background())
			info, statErr := os.Stat(path)
			if err != nil || statErr != nil || string(body) != "first snapshot" || info.Size() != int64(len(base)) {
				t.Fatal("torn tail not durably reclaimed", err, statErr)
			}
			if err := store.Save(context.Background(), []byte("replacement")); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := diskStore(t, dir)
			body, err = reopened.Load(context.Background())
			if err != nil || string(body) != "replacement" {
				t.Fatal(err)
			}
		})
	}
	for _, mode := range []string{"empty", "file_magic", "frame_magic", "length", "generation", "header_hash", "payload", "digest", "commit", "old_frame", "bad_tail"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, journalName)
			body := append(append([]byte(nil), base...), next...)
			offset := len(base)
			switch mode {
			case "empty":
				body = nil
			case "file_magic":
				body[0] ^= 1
			case "frame_magic":
				body[offset] ^= 1
			case "length":
				body[offset+7] ^= 1
			case "generation":
				body[offset+15] ^= 1
			case "header_hash":
				body[offset+16] ^= 1
			case "payload":
				body[offset+frameHeader] ^= 1
			case "digest":
				body[len(body)-len(commitMagic)-1] ^= 1
			case "commit":
				body[len(body)-1] ^= 1
			case "old_frame":
				body[len(journalMagic)+frameHeader] ^= 1
			case "bad_tail":
				body = append(body, '!')
			}
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			if store, err := OpenFileStore(context.Background(), dir); store != nil || !errors.Is(err, ErrSnapshot) {
				t.Fatal("corrupt journal loaded", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, body) {
				t.Fatal("corruption was silently repaired", err)
			}
			// Failed open must release its lock so the fixture can be repaired.
			if err := os.WriteFile(path, base, 0600); err != nil {
				t.Fatal(err)
			}
			diskStore(t, dir)
		})
	}
}

type sizedInfo struct {
	os.FileInfo
	size int64
}

func (i sizedInfo) Size() int64 { return i.size }

func TestFileStoreRecoveryAndContextFaults(t *testing.T) {
	ctx := context.Background()
	cause := errors.New("PRIVATE_IO")
	for _, mode := range []string{"read", "truncate", "recovery_sync", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			store := diskStore(t, dir)
			original := store.file
			if err := store.Save(ctx, []byte("known")); err != nil {
				t.Fatal(err)
			}
			if _, err := original.WriteAt(encodeJournalFrame(2, []byte("torn"))[:20], store.end); err != nil {
				t.Fatal(err)
			}
			wrapper := &faultJournal{journalFile: original}
			switch mode {
			case "read":
				wrapper.read = func([]byte, int64) (int, error) { return 0, cause }
			case "truncate":
				wrapper.truncate = func(int64) error { return cause }
			case "recovery_sync":
				wrapper.syncFn = func() error { return cause }
			case "oversized":
				wrapper.stat = func() (os.FileInfo, error) {
					info, err := original.Stat()
					return sizedInfo{info, MaxJournalBytes + 1}, err
				}
			}
			if body, _, _, err := recoverJournal(ctx, wrapper); body != nil || err == nil || (mode != "oversized" && !errors.Is(err, cause)) {
				t.Fatal("failed recovery exposed snapshot", err)
			}
		})
	}
	dir := t.TempDir()
	store := diskStore(t, dir)
	if _, err := OpenFileStore(nil, dir); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	if _, err := OpenFileStore(ctx, ""); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := OpenFileStore(canceled, dir); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := store.Save(canceled, []byte("ignored")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := store.Load(nil); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	if err := store.Save(ctx, nil); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	if err := store.Save(ctx, make([]byte, MaxSnapshotBytes+1)); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	original := store.file
	local, cancel := context.WithCancel(ctx)
	store.file = &faultJournal{journalFile: original, write: func(b []byte, o int64) (int, error) { n, err := original.WriteAt(b, o); cancel(); return n, err }}
	if err := store.Save(local, bytes.Repeat([]byte("x"), journalChunk*2)); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrStoreUncertain) {
		t.Fatal("mid-write cancellation not retained", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := diskStore(t, dir)
	body, err := reopened.Load(ctx)
	if err != nil || body != nil {
		t.Fatal("partial first record mistaken for snapshot", err)
	}
}

func TestFileStoreCloseWaitsForSaveAndCachesErrors(t *testing.T) {
	dir := t.TempDir()
	store := diskStore(t, dir)
	original := store.file
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	store.file = &faultJournal{journalFile: original, syncFn: func() error { close(entered); <-release; return original.Sync() }}
	saved := make(chan error, 1)
	closed := make(chan error, 1)
	go func() { saved <- store.Save(context.Background(), []byte("saved")) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Sync not entered")
	}
	go func() { closed <- store.Close() }()
	if _, err := OpenFileStore(context.Background(), dir); !errors.Is(err, ErrStoreBusy) {
		t.Fatal("lock released while Save active", err)
	}
	select {
	case <-closed:
		t.Fatal("Close skipped in-flight save")
	default:
	}
	unblock()
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	original = reopened.file
	cause := errors.New("PRIVATE_CLOSE")
	reopened.file = &faultJournal{journalFile: original, closeFn: func() error { return errors.Join(original.Close(), cause) }}
	if err := reopened.Close(); !errors.Is(err, cause) || strings.Contains(fmt.Sprintf("%#v", err), "PRIVATE") {
		t.Fatal(err)
	}
	if err := reopened.Close(); !errors.Is(err, cause) {
		t.Fatal("close error lost", err)
	}
	// This injected error followed actual closure; the successor can acquire.
	diskStore(t, dir)
}

func TestFileStoreRejectsNonRegularPathsAndChangedFiles(t *testing.T) {
	for _, name := range []string{lockName, journalName} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
		if s, err := OpenFileStore(context.Background(), dir); s != nil || !errors.Is(err, ErrSnapshot) {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	store := diskStore(t, dir)
	if err := store.Save(context.Background(), []byte("known")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.file.WriteAt([]byte("!"), store.end); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); !errors.Is(err, ErrStoreUncertain) || !errors.Is(err, ErrSnapshot) {
		t.Fatal("external size change ignored", err)
	}
	// Path/diagnostic details remain available only through explicit unwrapping.
	if !reflect.TypeOf(store).Implements(reflect.TypeOf((*SnapshotStore)(nil)).Elem()) {
		t.Fatal("store interface mismatch")
	}
}

func TestFileStoreKnownSyncSuccessAndInvalidLatestSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := diskStore(t, dir)
	sender := diskSender(t, store)
	e := simQueue(t, sender, 1)
	original := store.file
	ctx, cancel := context.WithCancel(context.Background())
	store.file = &faultJournal{journalFile: original, syncFn: func() error { err := original.Sync(); cancel(); return err }}
	a, err := sender.BeginSubmit(ctx, e.ClientMessageID)
	if err != nil || ctx.Err() == nil || a.Request != e.Request {
		t.Fatal("known Sync success lost", err)
	}
	store.file = original
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := diskStore(t, dir)
	if simEntry(t, diskSender(t, reopened)).State != PublishUnknown {
		t.Fatal("known submitted checkpoint missing")
	}
	// A checksum-valid but semantically damaged latest snapshot must not fall
	// back to an earlier state that could lose a terminal or original key.
	if err := reopened.Save(context.Background(), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	last := diskStore(t, dir)
	if s, err := OpenSender(context.Background(), 12, last); s != nil || !errors.Is(err, ErrSnapshot) {
		t.Fatal("invalid latest snapshot fell back", err)
	}
}
