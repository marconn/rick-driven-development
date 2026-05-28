package backend

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Smoke tests for the Antigravity (`agy`) driver. Unlike the buildArgs unit
// tests in backend_test.go, these exercise the full Run() chain end-to-end:
// exec.CommandContext, stdin piping, stdout capture, stderr-tail population
// on failure, and BackendError construction on context cancellation. A
// POSIX shell script stands in for the real `agy` binary so no upstream CLI
// is needed and the tests stay hermetic. Same fake-binary pattern used by
// TestClaude_Run_IdleTimeout_* in claude_idle_test.go.

// TestAntigravity_Run_Smoke_HappyPath: plain-text stdout from the fake CLI
// is captured verbatim into Response.Output. Unlike Claude/Gemini/Codex, the
// agy driver does NOT parse a stream-json envelope — it ships whatever the
// CLI prints. A regression here would break every persona running on the
// antigravity backend.
func TestAntigravity_Run_Smoke_HappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script-based fake binary is unix-only")
	}

	script := `#!/bin/sh
echo 'antigravity smoke: ok'
`
	binPath := writeFakeBinary(t, "fake-agy-happy.sh", script)

	a := NewAntigravity(binPath)
	resp, err := a.Run(context.Background(), Request{
		SystemPrompt: "sys",
		UserPrompt:   "hello",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp == nil {
		t.Fatal("want non-nil response")
	}
	if !strings.Contains(resp.Output, "antigravity smoke: ok") {
		t.Errorf("Output = %q; want it to contain CLI stdout", resp.Output)
	}
	if resp.Duration <= 0 {
		t.Errorf("Duration = %s; want positive elapsed", resp.Duration)
	}
	// agy has no documented stream-json envelope — StopReason / TokensUsed
	// must stay empty, not be invented. Pinning this prevents a future
	// driver "improvement" from silently fabricating stop reasons.
	if resp.StopReason != "" {
		t.Errorf("StopReason = %q; want empty (no stream envelope)", resp.StopReason)
	}
	if resp.TokensUsed != 0 {
		t.Errorf("TokensUsed = %d; want 0 (no usage reporting)", resp.TokensUsed)
	}
}

// TestAntigravity_Run_Smoke_LargePromptUsesStdin: when the combined prompt
// exceeds maxArgSize (128KB), buildArgs moves the prompt body to stdin and
// leaves bare `-p` in argv. This test proves that the body actually reaches
// the subprocess via stdin — script `cat`s stdin to stdout, and we look
// for a marker that was only ever in the prompt body. A regression here
// would silently send empty prompts to agy on any large-context persona
// (researcher, architect, pr-consolidator).
func TestAntigravity_Run_Smoke_LargePromptUsesStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script-based fake binary is unix-only")
	}

	script := `#!/bin/sh
cat
`
	binPath := writeFakeBinary(t, "fake-agy-stdin.sh", script)

	const marker = "STDIN_MARKER_4f8c1a"
	// Pad past maxArgSize so buildArgs takes the stdin branch. The marker
	// is embedded so a successful round-trip proves the body arrived.
	bigUserPrompt := strings.Repeat("x", maxArgSize) + "\n" + marker + "\n"

	a := NewAntigravity(binPath)
	resp, err := a.Run(context.Background(), Request{
		SystemPrompt: "sys",
		UserPrompt:   bigUserPrompt,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(resp.Output, marker) {
		t.Errorf("Output missing stdin marker %q — prompt body never reached subprocess", marker)
	}
}

// TestAntigravity_Run_Smoke_ModelDoesNotCrash: the real `agy` (v1.0.3) has no
// model flag and exits 2 with "flags provided but not defined: -m" the moment
// it sees one. This fake reproduces that fail-closed behavior, then asserts a
// Run with Request.Model set still SUCCEEDS — i.e. the driver dropped the
// model instead of forwarding it. This is the end-to-end guard for the bug
// that broke every antigravity call under a global RICK_MODEL or a
// rick_consult/rick_run model arg. A regression that re-adds `-m` turns this
// red with the exact stderr operators were hitting in production.
func TestAntigravity_Run_Smoke_ModelDoesNotCrash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script-based fake binary is unix-only")
	}

	// Mimic agy's flag parser: enumerate argv, reject -m/--model like the
	// stdlib flag package does, otherwise print OK.
	script := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    -m|--model|--model=*)
      echo "flags provided but not defined: $arg" 1>&2
      exit 2
      ;;
  esac
done
echo 'antigravity ok'
`
	binPath := writeFakeBinary(t, "fake-agy-model.sh", script)

	a := NewAntigravity(binPath)
	resp, err := a.Run(context.Background(), Request{
		SystemPrompt: "sys",
		UserPrompt:   "hello",
		Model:        "gemini-2.5-pro", // would crash a -m-forwarding driver
		Yolo:         true,
	})
	if err != nil {
		t.Fatalf("Run with Model set must succeed (model is dropped, not forwarded): %v", err)
	}
	if !strings.Contains(resp.Output, "antigravity ok") {
		t.Errorf("Output = %q; want the fake CLI's success line", resp.Output)
	}
}

// TestAntigravity_Run_Smoke_NonZeroExitCapturesStderr: a non-zero CLI exit
// must surface as a BackendError carrying Backend="antigravity" and the
// stderr tail. Operators rely on this stderr text in rick_persona_output
// to diagnose CLI-side auth failures, quota errors, and bad-model rejections
// — losing it would push them back to subprocess logs.
func TestAntigravity_Run_Smoke_NonZeroExitCapturesStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script-based fake binary is unix-only")
	}

	script := `#!/bin/sh
echo 'agy: authentication failed' 1>&2
exit 7
`
	binPath := writeFakeBinary(t, "fake-agy-fail.sh", script)

	a := NewAntigravity(binPath)
	resp, err := a.Run(context.Background(), Request{
		SystemPrompt: "sys",
		UserPrompt:   "hello",
	})
	if resp != nil {
		t.Fatalf("want nil response on subprocess failure, got %#v", resp)
	}
	if err == nil {
		t.Fatal("want error on non-zero exit, got nil")
	}

	var backendErr *BackendError
	if !errors.As(err, &backendErr) {
		t.Fatalf("want *BackendError, got %T: %v", err, err)
	}
	if backendErr.Backend != "antigravity" {
		t.Errorf("Backend = %q; want antigravity", backendErr.Backend)
	}
	if !strings.Contains(backendErr.Stderr, "authentication failed") {
		t.Errorf("Stderr = %q; want captured stderr tail", backendErr.Stderr)
	}
	if backendErr.Duration <= 0 {
		t.Errorf("Duration = %s; want positive elapsed", backendErr.Duration)
	}
}

// TestAntigravity_Run_Smoke_ContextCancellation: parent ctx cancel must
// terminate the subprocess and return a BackendError whose Inner unwraps
// to context.Canceled. This is the path PersonaRunner takes when a
// workflow is paused or a job is killed; without errors.Is matching, the
// classifier mislabels cancellations as handler_error and they get retried.
func TestAntigravity_Run_Smoke_ContextCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script-based fake binary is unix-only")
	}

	// Long sleep so the parent context is what wins. configureProcessGroup
	// SIGKILLs the whole group, so the 30s sleep never actually elapses.
	script := `#!/bin/sh
echo '{"type":"system"}'
sleep 30
`
	binPath := writeFakeBinary(t, "fake-agy-cancel.sh", script)

	a := NewAntigravity(binPath)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after Run starts — long enough for exec to spin up the
	// shell, short enough that we don't pay the full sleep on a regression.
	time.AfterFunc(150*time.Millisecond, cancel)

	start := time.Now()
	resp, err := a.Run(ctx, Request{
		SystemPrompt: "sys",
		UserPrompt:   "hello",
	})
	elapsed := time.Since(start)

	if resp != nil {
		t.Fatalf("want nil response on ctx cancel, got %#v", resp)
	}
	if err == nil {
		t.Fatal("want error on ctx cancel, got nil")
	}
	// 7s ceiling matches the grandchild-kill test in claude_idle_test.go:
	// allows defaultKillGraceDelay (5s) plus CI slack. A regression that
	// orphans the child would push elapsed near 30s.
	if elapsed > 7*time.Second {
		t.Errorf("Run took %s — subprocess outlived ctx cancel", elapsed)
	}

	var backendErr *BackendError
	if !errors.As(err, &backendErr) {
		t.Fatalf("want *BackendError, got %T: %v", err, err)
	}
	if backendErr.Backend != "antigravity" {
		t.Errorf("Backend = %q; want antigravity", backendErr.Backend)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; classifyDispatchFailure will mislabel cancellation as handler_error")
	}
}
