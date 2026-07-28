package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// MigrateCredentials migrates credentials from the old per-file layout
// ($DataDir/credentials/<provider>/<type>.json) to the consolidated
// credentials.json file. It is safe to call multiple times — it's a no-op
// if the old directory doesn't exist or has already been migrated.
func MigrateCredentials() error {
	oldDir := credentialsDir()
	if _, err := os.Stat(oldDir); os.IsNotExist(err) {
		return nil
	}

	credentialsMu.Lock()
	defer credentialsMu.Unlock()

	store, err := loadCredentialsStore()
	if err != nil {
		return err
	}

	migrated := false
	readErrors := false
	entries, err := os.ReadDir(oldDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // vanished between Stat and ReadDir
		}
		return fmt.Errorf("reading legacy credentials directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		providerID := entry.Name()
		providerDir := filepath.Join(oldDir, providerID)

		files, err := os.ReadDir(providerDir)
		if err != nil {
			readErrors = true
			continue
		}

		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			credType := strings.TrimSuffix(f.Name(), ".json")

			// Don't overwrite credentials already in the consolidated file
			if store[providerID] != nil {
				if _, exists := store[providerID][credType]; exists {
					continue
				}
			}

			data, err := os.ReadFile(filepath.Join(providerDir, f.Name()))
			if err != nil {
				readErrors = true
				continue
			}

			// Validate it's valid JSON before storing
			if !json.Valid(data) {
				continue
			}

			if store[providerID] == nil {
				store[providerID] = make(map[string]json.RawMessage)
			}
			store[providerID][credType] = json.RawMessage(data)
			migrated = true
		}
	}

	if migrated {
		if err := saveCredentialsStore(store); err != nil {
			return err
		}
	}

	// Only clean up the old directory if all files were read successfully.
	// If some files couldn't be read (permissions, I/O errors), leave them
	// so the next migration attempt can pick them up.
	if !readErrors {
		_ = os.RemoveAll(oldDir)
	} else {
		return fmt.Errorf("some legacy credential files could not be read; they were left in %s for the next attempt", oldDir)
	}

	return nil
}
