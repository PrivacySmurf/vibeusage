//go:build !windows

package testenv_test

import (
	"syscall"
	"testing"
)

// TestBoundedRepeatedFixtures_DoNotExhaustDescriptors runs 80 sequential child
// subtests / 160 socket allocations (SetClosedCDPPort + ApplyVibeusage per
// child) under a 64-FD RLIMIT_NOFILE budget.
func TestBoundedRepeatedFixtures_DoNotExhaustDescriptors(t *testing.T) {
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
		t.Fatalf("Getrlimit(RLIMIT_NOFILE): %v", err)
	}
	limit := old
	limit.Cur = 64
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("Setrlimit(RLIMIT_NOFILE, 64): %v", err)
	}
	defer func() {
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old)
	}()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("repeated fixtures exhausted 64-FD budget: %v", r)
		}
	}()

	for range 80 {
		t.Run("iter", func(t *testing.T) {
			_ = invokeSetClosedCDPPort(t)
			_ = invokeApplyVibeusage(t, t.TempDir())
		})
	}
}
