//go:build darwin

package workspace

import (
	"errors"
	"fmt"
	"log/slog"

	"golang.org/x/sys/unix"
)

// cloneTree clones srcRepo into destPath. On macOS this issues a single
// clonefile(2) syscall: APFS performs a copy-on-write clone of the entire
// directory hierarchy in O(1) regardless of size, sharing data blocks until
// either side is modified. Falls back to cpFallback when the underlying
// filesystem refuses the clone (cross-volume or non-APFS dest).
func cloneTree(srcRepo, destPath string) error {
	err := unix.Clonefile(srcRepo, destPath, 0)
	if err == nil {
		return nil
	}
	// EXDEV: src and dst on different volumes. ENOTSUP: dest filesystem
	// doesn't implement clonefile (e.g. non-APFS mount under RICK_REPOS_PATH).
	// Both are operator-config issues, not bugs — degrade to a byte copy
	// rather than failing setup.
	if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTSUP) {
		// Surface the degraded path so operators can spot a misconfigured
		// RICK_REPOS_PATH (e.g. mounted on a non-APFS volume) — otherwise
		// the symptom is just "workspace setup feels slow" with no clue.
		slog.Warn("workspace: APFS clonefile unavailable, falling back to cp -r",
			"src", srcRepo, "dst", destPath, "reason", err)
		return cpFallback(srcRepo, destPath)
	}
	return fmt.Errorf("clonefile: %w", err)
}
