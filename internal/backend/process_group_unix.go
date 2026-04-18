//go:build unix

package backend

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// defaultKillGraceDelay bounds how long cmd.Wait blocks on stdio pipes
// after the process group has been signalled. Even with pgroup-wide
// SIGKILL a process in uninterruptible sleep, or a descendant that
// escaped the group via setsid, could keep pipes open indefinitely. When
// waitDelay expires the io.Copy goroutines that drain stdout/stderr are
// force-stopped and cmd.Wait returns.
//
// Five seconds is arbitrary but comfortably bigger than any expected
// graceful-death window for an LLM CLI, and small enough that an operator
// isn't wondering why cancel took so long.
const defaultKillGraceDelay = 5 * time.Second

// configureProcessGroup makes cmd's lifecycle robust to grandchild
// processes the wrapped CLI spawns (claude → node, gemini → node, shell
// scripts → sleep, etc.).
//
// Problem this solves — the developer-zero-iteration shape documented in
// planning-workspace/rick-feedback-2026-04-18-developer-zero-iteration.md:
// exec.CommandContext's default cancellation calls cmd.Process.Kill, which
// SIGKILLs only the direct child. Descendants survive and keep stdio pipes
// open, so cmd.Wait blocks on the io.Copy goroutines until every
// descendant exits naturally. In production this produced 9-minute wall
// durations against a 2-minute idle watchdog.
//
// Fix:
//  1. Setpgid=true — new process group rooted at the child's PID.
//  2. Cancel — syscall.Kill(-pgid, SIGKILL) fans SIGKILL out to every
//     process in that group. ESRCH (no such process) maps to ErrProcessDone
//     so exec.Cmd treats it as a benign race, not a kill failure.
//  3. WaitDelay — if any descendant somehow outlives the group kill,
//     cmd.Wait force-closes the I/O pipes after this delay and returns.
func configureProcessGroup(cmd *exec.Cmd, waitDelay time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// Negative PID targets the whole process group.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = waitDelay
}
