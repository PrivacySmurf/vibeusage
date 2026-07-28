package history

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
)

func withHistoryLock(providerID string, fn func() error) error {
	return withProviderHistoryLock(providerID, true, fn)
}

func withHistoryReadLock(providerID string, fn func() error) error {
	return withProviderHistoryLock(providerID, false, fn)
}

func withProviderHistoryLock(providerID string, exclusive bool, fn func() error) error {
	lockDir := filepath.Join(config.DataDir(), ".history-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return fmt.Errorf("creating history lock directory: %w", err)
	}
	return withFileLock(filepath.Join(lockDir, "all.lock"), false, "history", func() error {
		return withFileLock(filepath.Join(lockDir, providerID+".lock"), exclusive, "history for "+providerID, fn)
	})
}

func withAllHistoryLock(fn func() error) error {
	lockDir := filepath.Join(config.DataDir(), ".history-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return fmt.Errorf("creating history lock directory: %w", err)
	}
	return withFileLock(filepath.Join(lockDir, "all.lock"), true, "history", fn)
}

func withFileLock(path string, exclusive bool, description string, fn func() error) (err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s lock: %w", description, err)
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("closing %s lock: %w", description, closeErr)
		}
	}()

	if err := lockFile(file, exclusive); err != nil {
		return fmt.Errorf("locking %s: %w", description, err)
	}
	defer func() {
		if unlockErr := unlockFile(file); err == nil && unlockErr != nil {
			err = fmt.Errorf("unlocking %s: %w", description, unlockErr)
		}
	}()

	return fn()
}
