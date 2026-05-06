package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestClaude_Run_IdleTimeout_SurfacesBackendError reproduces the zero-iteration
// developer-phase failure documented in
// planning-workspace/rick-feedback-2026-04-18-developer-zero-iteration.md: a
// backend subprocess emits some bootstrap noise, then goes silent while the
// caller is waiting for tokens, and the idle watchdog must kill it and return
// a BackendError that carries Inner=ErrIdleTimeout plus the captured stderr
// tail so operators can diagnose via rick_persona_output.
//
// This is a hermetic repro: we stand in for the real `claude` CLI with a
// shell script that writes a few bytes to stdout AND stderr, then sleeps
// longer than the idle watchdog window. A production-shape failure is
// identical — the real CLI prints its `system`/`init` events, then the
// upstream Anthropic stream never delivers its first content token before the
// 2-minute watchdog fires.
func TestClaude_Run_IdleTimeout_SurfacesBackendError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("claude backend is linux/macos only — script-based fake binary not portable to windows")
	}

	// Script writes one line to stdout, one to stderr, then sleeps well
	// past the idle window so the watchdog has to kill it. The sleep is 5s
	// so the test doesn't hang if the watchdog regresses — the
	// cmd.CommandContext kill path should terminate it in milliseconds
	// after stallTimeout elapses.
	// Plain `sleep 5` runs as a grandchild of the shell — this is the
	// production shape (claude CLI forks node). configureProcessGroup
	// must SIGKILL the whole process group so cmd.Wait doesn't block on
	// sleep's inherited stdio pipes.
	script := `#!/bin/sh
echo '{"type":"system","subtype":"init"}'
echo 'bootstrapping claude wrapper' 1>&2
sleep 5
`
	binPath := writeFakeBinary(t, "fake-claude.sh", script)

	c := NewClaude(binPath)
	c.stallTimeout = 150 * time.Millisecond

	start := time.Now()
	resp, err := c.Run(context.Background(), Request{
		SystemPrompt: "sys",
		UserPrompt:   "hello",
	})
	elapsed := time.Since(start)

	if resp != nil {
		t.Fatalf("want nil response on idle timeout, got %#v", resp)
	}
	if err == nil {
		t.Fatal("want error on idle timeout, got nil")
	}

	// Ceiling of 4s: plenty of slack for a loaded CI runner; anything
	// close to the 5s sleep means the subprocess outlived the watchdog.
	if elapsed > 4*time.Second {
		t.Errorf("Run blocked for %s — watchdog did not kill subprocess", elapsed)
	}

	var backendErr *BackendError
	if !errors.As(err, &backendErr) {
		t.Fatalf("want *BackendError, got %T: %v", err, err)
	}
	if backendErr.Backend != "claude" {
		t.Errorf("Backend = %q; want claude", backendErr.Backend)
	}
	if !errors.Is(err, ErrIdleTimeout) {
		t.Errorf("errors.Is(err, ErrIdleTimeout) = false; failure_classify will mislabel this as handler_error")
	}
	// Stall duration must travel with the error so the message contains
	// "(stall=150ms)" — operators grep for this to confirm the watchdog
	// (not the wall-clock deadline) fired.
	if !strings.Contains(backendErr.Inner.Error(), "stall=") {
		t.Errorf("Inner = %q; want stall=<duration> marker", backendErr.Inner.Error())
	}
	if backendErr.Duration <= 0 {
		t.Errorf("Duration = %s; want positive elapsed", backendErr.Duration)
	}
	// The bootstrap stderr line MUST survive to the BackendError. This is
	// the single most-requested diagnostic from the operator report: the
	// 4KB stderr tail is the only way to see what the subprocess did
	// before it went silent.
	if !strings.Contains(backendErr.Stderr, "bootstrapping claude wrapper") {
		t.Errorf("Stderr = %q; want captured bootstrap stderr", backendErr.Stderr)
	}
}

// TestClaude_Run_IdleTimeout_PreservesErrorMessageShape pins the stable
// error string that downstream surfaces (rick_persona_output, agent UI
// hover, operator grep) depend on. Regressions here would silently break
// the diagnostic reporting that was shipped in commit 51aabc8.
func TestClaude_Run_IdleTimeout_PreservesErrorMessageShape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("claude backend is linux/macos only")
	}

	script := `#!/bin/sh
echo '{}'
sleep 3
`
	binPath := writeFakeBinary(t, "fake-claude-shape.sh", script)

	c := NewClaude(binPath)
	c.stallTimeout = 120 * time.Millisecond

	_, err := c.Run(context.Background(), Request{UserPrompt: "x"})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	// The message format this guards:
	//   claude: backend: idle timeout exceeded (stall=120ms) (after 150ms)
	// classifyDispatchFailure doesn't parse this, but humans and
	// rick_persona_output's Error field do.
	for _, needle := range []string{"claude", "idle timeout", "stall=", "after"} {
		if !strings.Contains(msg, needle) {
			t.Errorf("error message %q missing %q — stable format regressed", msg, needle)
		}
	}
}

// TestClaude_Run_IdleTimeout_KillsGrandchildren is the explicit regression
// guard for the grandchild-survives-SIGKILL bug that configureProcessGroup
// was written to fix. The script forks a background sleep AND exits the
// shell immediately — so the grandchild is reparented to init and keeps
// the original stdout pipe open. Before the fix, cmd.Wait would block for
// the full grandchild sleep duration even though the direct child had
// exited; after the fix, the whole process group is killed and Run
// returns in well under the sleep window.
func TestClaude_Run_IdleTimeout_KillsGrandchildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group kill is unix-only")
	}

	// `sleep 10 &` backgrounds the sleep (grandchild of cmd). The parent
	// shell then emits the final line and waits on the sleep via `wait`
	// so the shell is still alive when the watchdog fires — mimicking a
	// CLI that's waiting on its own subprocess. The 10s ceiling is
	// intentionally long: a regression would push elapsed past 5s and
	// trip the assert below.
	script := `#!/bin/sh
echo '{"type":"system"}'
sleep 10 &
echo 'parent waiting' 1>&2
wait
`
	binPath := writeFakeBinary(t, "fake-claude-grandchild.sh", script)

	c := NewClaude(binPath)
	c.stallTimeout = 150 * time.Millisecond

	start := time.Now()
	_, err := c.Run(context.Background(), Request{UserPrompt: "x"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want error on idle timeout")
	}
	// Allow up to defaultKillGraceDelay (5s) + slack for the WaitDelay
	// fallback. A working process-group kill should finish in well under
	// 1s — the slack is for loaded CI runners. Anything near 10s means
	// the grandchild was orphaned and we're back to the production bug.
	if elapsed > 7*time.Second {
		t.Errorf("Run took %s — grandchild sleep was not killed by process-group SIGKILL", elapsed)
	}
	if !errors.Is(err, ErrIdleTimeout) {
		t.Errorf("err = %v; want ErrIdleTimeout", err)
	}
}

// TestClaude_Run_ToolExecutionPhaseDoesNotIdleTimeout pins the post-2026-05-06
// contract: any stdout byte from the CLI counts as progress, including
// tool_use blocks, input_json_delta frames, system.task_started/notification
// events, and user/tool_result echoes — none of which contain a text_delta
// but all of which are real signals that the CLI is alive. Earlier revisions
// gated progress on content_block_delta→text_delta only; that misclassified
// long-tool-execution windows (Bash / Task / MCP) as wedges and SIGKILL'd
// healthy subprocesses. The CLI's own internal stream watchdog (~45s with
// 5min full-abort+retry) is the authoritative wedge detector now.
func TestClaude_Run_ToolExecutionPhaseDoesNotIdleTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("claude backend is linux/macos only — script-based fake binary not portable to windows")
	}

	// Emit tool-execution-shape stdout (matches what the real CLI 2.1.131
	// emits during a Bash tool run) at 80ms intervals across a window that
	// exceeds the stallTimeout. None of these frames carry a text_delta;
	// the watchdog must NOT fire because raw stdout is active.
	script := `#!/bin/sh
i=0
while [ $i -lt 12 ]; do
  printf '{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","id":"tu_01","name":"Bash"}}}\n'
  printf '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"x"}}}\n'
  printf '{"type":"system","subtype":"task_started"}\n'
  printf '{"type":"system","subtype":"task_notification","status":"completed"}\n'
  sleep 0.08
  i=$((i+1))
done
printf '{"type":"result","subtype":"success","stop_reason":"end_turn","result":"ok","usage":{"input_tokens":1,"output_tokens":1}}\n'
`
	binPath := writeFakeBinary(t, "fake-claude-tool-exec.sh", script)

	c := NewClaude(binPath)
	// 300ms stall + 80ms inter-frame gap: pre-fix would idle-timeout because
	// none of these frames are text_delta. Post-fix, the script runs to
	// completion and Run returns nil error.
	c.stallTimeout = 300 * time.Millisecond

	start := time.Now()
	resp, err := c.Run(context.Background(), Request{
		SystemPrompt: "sys",
		UserPrompt:   "hello",
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run returned error during healthy tool-execution stream: %v (after %s)", err, elapsed)
	}
	if resp == nil {
		t.Fatal("want non-nil response on successful run, got nil")
	}
	// Sanity: must have actually run for at least a few intervals — otherwise
	// the script exited before producing the tool-execution noise we're
	// guarding.
	if elapsed < 500*time.Millisecond {
		t.Errorf("Run completed in %s — script likely didn't exercise the tool-exec window", elapsed)
	}
}

// writeFakeBinary drops a POSIX shell script into t.TempDir and returns its
// absolute path. The script becomes the "claude binary" passed to NewClaude
// — the real Run() still exec's it as a child process, so the full chain
// (exec.CommandContext → StreamWriter → WithIdleTimeout → BackendError)
// is exercised end-to-end without touching the real claude CLI.
func writeFakeBinary(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}
