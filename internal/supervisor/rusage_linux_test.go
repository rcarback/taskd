// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"syscall"
	"testing"
)

// TestMaxRSSBytesOnLinux pins the platform-specific conversion so swapping
// this file's implementation with rusage_darwin.go's (or vice versa) fails a
// test instead of passing silently, as it did before this test existed:
// Linux's Rusage.Maxrss is reported in kilobytes.
func TestMaxRSSBytesOnLinux(t *testing.T) {
	got := maxRSSBytes(&syscall.Rusage{Maxrss: 1024})
	if want := int64(1024 * 1024); got != want {
		t.Fatalf("maxRSSBytes(Maxrss: 1024) = %d, want %d (Linux reports kilobytes)", got, want)
	}
}
