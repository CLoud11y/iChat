package clientsim

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
)

var (
	ErrStoreBusy        = errors.New("simulator store already owned")
	ErrStoreClosed      = errors.New("simulator store closed")
	ErrStoreUnsupported = errors.New("simulator file locking unsupported")
)

const (
	MaxJournalBytes int64 = 256 * 1024 * 1024
	journalName           = "sender.journal"
	lockName              = "sender.lock"
)

type journalFile interface {
	io.ReaderAt
	io.WriterAt
	Stat() (os.FileInfo, error)
	Truncate(int64) error
	Sync() error
	Close() error
}

// FileStore owns a lock and append-only journal in an existing private local
// directory. One Sender exclusively borrows it until retired; close it only
// after all users have returned. Do not externally replace its files while open.
type FileStore struct {
	mu              sync.Mutex
	root            *os.Root
	lock            *os.File
	file            journalFile
	last            []byte
	end             int64
	generation      uint64
	faulted, closed bool
	closeErr        error
}

func (*FileStore) String() string     { return "simulator file store (content and path redacted)" }
func (s *FileStore) GoString() string { return s.String() }

// OpenFileStore never creates the directory or removes/replaces an existing
// journal. It acquires the kernel lock before opening or recovering data.
func OpenFileStore(ctx context.Context, directory string) (_ *FileStore, err error) {
	if ctx == nil || directory == "" {
		return nil, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, safeError(ErrStore, err)
	}
	s := &FileStore{root: root}
	defer func() {
		if err != nil {
			err = safeError(ErrStore, errors.Join(err, s.Close()))
		}
	}()
	if err = regularStorePath(root, lockName); err != nil {
		return nil, err
	}
	s.lock, err = root.OpenFile(lockName, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, safeError(ErrStore, err)
	}
	if err = lockStoreFile(s.lock); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = regularStorePath(root, journalName); err != nil {
		return nil, err
	}
	f, openErr := root.OpenFile(journalName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	created := openErr == nil
	if errors.Is(openErr, os.ErrExist) {
		f, openErr = root.OpenFile(journalName, os.O_RDWR, 0600)
	}
	if openErr != nil {
		return nil, safeError(ErrStore, openErr)
	}
	s.file = f
	if created {
		if err = writeJournalAt(ctx, f, []byte(journalMagic), 0); err != nil {
			return nil, safeError(ErrStore, err)
		}
		if err = f.Sync(); err != nil {
			return nil, safeError(ErrStore, err)
		}
	}
	s.last, s.end, s.generation, err = recoverJournal(ctx, f)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func regularStorePath(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return safeError(ErrStore, err)
	}
	if !info.Mode().IsRegular() {
		return ErrSnapshot
	}
	return nil
}

func (s *FileStore) Load(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if err := s.checkSize(); err != nil {
		return nil, err
	}
	return append([]byte(nil), s.last...), nil
}

func (s *FileStore) Save(ctx context.Context, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return err
	}
	if len(body) == 0 {
		return ErrInput
	}
	if len(body) > MaxSnapshotBytes || s.generation == ^uint64(0) || s.end+int64(frameOverhead+len(body)) > MaxJournalBytes {
		return ErrCapacity
	}
	if err := s.checkSize(); err != nil {
		return err
	}
	frame := encodeJournalFrame(s.generation+1, body)
	// Once any write may have begun, errors/panics require close and reopen.
	s.faulted = true
	if err := writeJournalAt(ctx, s.file, frame, s.end); err != nil {
		return safeError(ErrStoreUncertain, err)
	}
	if err := ctx.Err(); err != nil {
		return safeError(ErrStoreUncertain, err)
	}
	if err := s.file.Sync(); err != nil {
		return safeError(ErrStoreUncertain, err)
	}
	s.last = append([]byte(nil), body...)
	s.end += int64(len(frame))
	s.generation++
	s.faulted = false
	return nil
}

func (s *FileStore) ready(ctx context.Context) error {
	if ctx == nil {
		return ErrInput
	}
	if s.closed {
		return ErrStoreClosed
	}
	if s.faulted {
		return ErrStoreUncertain
	}
	return ctx.Err()
}

func (s *FileStore) checkSize() error {
	info, err := s.file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != s.end {
		s.faulted = true
		return safeError(ErrStoreUncertain, errors.Join(ErrSnapshot, err))
	}
	return nil
}

// Close waits for synchronous Load/Save calls, then closes data before releasing
// the kernel lock. It does not delete the persistent lock file or journal.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	var errs []error
	if s.file != nil {
		errs = append(errs, s.file.Close())
	}
	if s.lock != nil {
		errs = append(errs, s.lock.Close())
	}
	if s.root != nil {
		errs = append(errs, s.root.Close())
	}
	if err := errors.Join(errs...); err != nil {
		s.closeErr = safeError(ErrStore, err)
	}
	return s.closeErr
}
