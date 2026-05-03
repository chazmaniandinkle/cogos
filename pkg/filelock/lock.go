// Package filelock provides advisory file locking using syscall.Flock.
//
// This package is used by pkg/alias and any other component that needs
// exclusive write access to node-local YAML files (aliases.yaml, global.yaml).
// The lock is advisory — callers that don't use filelock can still read and
// write the files. All cog CLI write paths go through this package.
//
// Lock lifecycle:
//
//	lock, err := filelock.Acquire(path, 2*time.Second)
//	if err != nil {
//	    return err  // ErrLockTimeout if another process holds the lock
//	}
//	defer lock.Release()
//	// ... read / mutate / atomic-write ...
package filelock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// ErrLockTimeout is returned when Acquire cannot obtain the lock within
// the requested timeout.
var ErrLockTimeout = errors.New("filelock: timed out waiting for lock")

// lockPollInterval is how often Acquire retries after a failed non-blocking
// flock attempt.
const lockPollInterval = 50 * time.Millisecond

// FileLock holds an open file descriptor that has been exclusively locked.
// Callers must call Release when finished.
type FileLock struct {
	file *os.File
}

// Acquire opens (or creates) the file at path and takes an exclusive advisory
// lock on it, retrying until the lock is obtained or timeout elapses.
//
// On darwin and Linux, the lock is implemented via syscall.Flock(LOCK_EX).
// The lock file itself is typically a dedicated ".lock" companion file next
// to the protected YAML, not the YAML file itself (though locking the YAML
// directly is also safe).
//
// Returns ErrLockTimeout if the lock cannot be obtained before timeout.
func Acquire(path string, timeout time.Duration) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("filelock: open %q: %w", path, err)
	}

	deadline := time.Now().Add(timeout)
	for {
		// LOCK_EX | LOCK_NB — exclusive, non-blocking.
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			// Lock acquired.
			return &FileLock{file: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			// Unexpected error — not just "another process holds the lock".
			f.Close()
			return nil, fmt.Errorf("filelock: flock %q: %w", path, err)
		}
		// Another process holds the lock. Wait and retry until deadline.
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("%w: %s", ErrLockTimeout, path)
		}
		time.Sleep(lockPollInterval)
	}
}

// Release unlocks the file and closes the descriptor. It is safe to call
// Release more than once; subsequent calls are no-ops.
func (l *FileLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	// Unlock first, then close. Flock releases automatically on close anyway,
	// but being explicit is clearer and avoids relying on that behaviour.
	if err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN); err != nil {
		// Best-effort: still try to close.
		l.file.Close()
		l.file = nil
		return fmt.Errorf("filelock: unlock: %w", err)
	}
	err := l.file.Close()
	l.file = nil
	return err
}
