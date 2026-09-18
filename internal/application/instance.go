package application

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var errDatabaseInstanceRunning = errors.New("another application instance is already using this database")

// databaseInstanceLock holds an OS file lock until all services using the
// database have stopped. The sidecar is deliberately retained on disk: removing
// it would let another process create and lock a different file at the same path.
type databaseInstanceLock struct {
	file     *os.File
	once     sync.Once
	closeErr error
}

// acquireDatabaseInstance must run before migrations or interrupted-task
// recovery. Locking a separate file leaves SQLite's own byte-range locks intact.
// A second handle is rejected even in the same process; there is no thread-owned
// mutex that can be re-entered after the Go scheduler moves a goroutine.
func acquireDatabaseInstance(dbPath string) (*databaseInstanceLock, error) {
	canonical, err := canonicalDatabasePath(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve database instance path: %w", err)
	}
	file, err := os.OpenFile(canonical+".instance.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open database instance lock: %w", err)
	}
	if err := lockInstanceFile(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock database %q: %w", canonical, err)
	}
	return &databaseInstanceLock{file: file}, nil
}

func (lock *databaseInstanceLock) Close() error {
	if lock == nil {
		return nil
	}
	lock.once.Do(func() {
		lock.closeErr = errors.Join(unlockInstanceFile(lock.file), lock.file.Close())
	})
	return lock.closeErr
}

// Resolve existing symlinks even when the database has not yet been created.
// Windows case aliases map to the same sidecar through the filesystem itself.
func canonicalDatabasePath(dbPath string) (string, error) {
	if strings.TrimSpace(dbPath) == "" {
		return "", errors.New("database path is empty")
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return "", err
	}
	probe := filepath.Clean(abs)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}
