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

// TestClaude_Run_IdleTimeout_ProtocolNoiseDoesNotResetWatchdog is the
// regression test for the 2026-04-18 idle_timeout operator bug: Claude CLI
// emits stream_event envelopes (tool_use, message_start, keep-alive pings)
// that previously reset the idle watchdog via newProgressWriter on raw stdout,
// even though no text was ever generated. A wedged model could therefore
// suppress the 2m watchdog for the full 9m wall-clock budget.
//
// The fix threads progress() into ClaudePrintExtractor so it fires only on
// content_block_delta→text_delta events and terminal result/message_delta
// events. This test verifies that a subprocess emitting ONLY non-text protocol
// frames still idle-times-out within the stall window even though stdout is
// actively chattering protocol noise.
func TestClaude_Run_IdleTimeout_ProtocolNoiseDoesNotResetWatchdog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("claude backend is linux/macos only — script-based fake binary not portable to windows")
	}

	// The script emits tool_use, message_start, and ping stream_event frames
	// in a tight loop — exactly the protocol noise the real Claude CLI emits
	// while waiting for an upstream Anthropic response. None of these frames
	// contain a text_delta, so the extractor must NOT fire progress() on them.
	// After the noise loop the script sleeps to simulate a permanently wedged
	// model. The watchdog must fire within stallTimeout regardless.
	script := `#!/bin/sh
# Emit protocol-noise frames (tool_use, message_start, ping) in a tight loop.
# None contain text_delta — the extractor must NOT reset the idle watchdog.
i=0
while [ $i -lt 20 ]; do
  printf '{"type":"stream_event","event":{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0}}}}\n'
  printf '{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","id":"tu_01","name":"bash"}}}\n'
  printf '{"type":"stream_event","event":{"type":"ping"}}\n'
  i=$((i+1))
done
# Then go silent — a permanently wedged generator.
sleep 10
`
	binPath := writeFakeBinary(t, "fake-claude-noise.sh", script)

	c := NewClaude(binPath)
	// Stall window must be short enough that the test finishes quickly.
	// The noise loop completes in well under 100ms on any machine; the
	// watchdog must fire after that without the noise having reset it.
	c.stallTimeout = 300 * time.Millisecond

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
		t.Fatal("want ErrIdleTimeout, got nil")
	}
	// Must time out well before the 10s sleep ends — generous 4s ceiling.
	if elapsed > 4*time.Second {
		t.Errorf("Run took %s — protocol noise reset the idle watchdog (regression of 2026-04-18 bug)", elapsed)
	}
	if !errors.Is(err, ErrIdleTimeout) {
		t.Errorf("want ErrIdleTimeout, got %v", err)
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
