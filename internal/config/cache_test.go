package config

import (
	"errors"
	"os"
	"testing"

	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

func TestClearModelsCacheRemovesAllCatalogFiles(t *testing.T) {
	testenv.SetCacheDir(t.Setenv, t.TempDir())

	for _, path := range []string{ModelsFile(), MultipliersFile()} {
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	ClearModelsCache()

	for _, path := range []string{ModelsFile(), MultipliersFile()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("catalog cache %s still exists: %v", path, err)
		}
	}
}
