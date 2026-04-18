//go:build !unix

package backend

import (
	"os/exec"
	"time"
)

// defaultKillGraceDelay is the non-unix fallback for how long cmd.Wait
// blocks on stdio after context cancellation. Rick targets linux/macos
// in production — this stub exists only to keep the package buildable
// on other platforms (CI matrices, contributor laptops).
const defaultKillGraceDelay = 5 * time.Second

// configureProcessGroup: non-unix stub. Without Setpgid + pgroup-wide
// kill we can't guarantee descendants die, so we fall back to WaitDelay
// alone — cmd.Wait will at least unblock after the delay even if the
// direct SIGKILL leaves orphans running.
func configureProcessGroup(cmd *exec.Cmd, waitDelay time.Duration) {
	cmd.WaitDelay = waitDelay
}
