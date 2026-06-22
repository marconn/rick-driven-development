package backend

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Factory
// ---------------------------------------------------------------------------

func TestNew(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		b, err := New("claude")
		if err != nil {
			t.Fatalf("New(claude): %v", err)
		}
		if b.Name() != "claude" {
			t.Errorf("want claude, got %s", b.Name())
		}
	})

	t.Run("gemini", func(t *testing.T) {
		b, err := New("gemini")
		if err != nil {
			t.Fatalf("New(gemini): %v", err)
		}
		if b.Name() != "gemini" {
			t.Errorf("want gemini, got %s", b.Name())
		}
	})

	t.Run("codex", func(t *testing.T) {
		b, err := New("codex")
		if err != nil {
			t.Fatalf("New(codex): %v", err)
		}
		if b.Name() != "codex" {
			t.Errorf("want codex, got %s", b.Name())
		}
	})

	t.Run("antigravity", func(t *testing.T) {
		b, err := New("antigravity")
		if err != nil {
			t.Fatalf("New(antigravity): %v", err)
		}
		if b.Name() != "antigravity" {
			t.Errorf("want antigravity, got %s", b.Name())
		}
	})

	t.Run("opencode", func(t *testing.T) {
		b, err := New("opencode")
		if err != nil {
			t.Fatalf("New(opencode): %v", err)
		}
		if b.Name() != "opencode" {
			t.Errorf("want opencode, got %s", b.Name())
		}
	})

	t.Run("unknown", func(t *testing.T) {
		_, err := New("openai")
		if err == nil {
			t.Fatal("want error for unknown backend")
		}
	})
}

// ---------------------------------------------------------------------------
// Claude buildArgs
// ---------------------------------------------------------------------------

func TestClaudeBuildArgs(t *testing.T) {
	c := NewClaude("claude")

	t.Run("basic", func(t *testing.T) {
		args, stdin := c.buildArgs(Request{
			SystemPrompt: "You are an expert.",
			UserPrompt:   "Hello",
		})
		assertContains(t, args, "-p")
		assertContains(t, args, "--system-prompt")
		assertContains(t, args, "--output-format")
		assertContains(t, args, "stream-json")
		assertContains(t, args, "--verbose")
		assertContains(t, args, "--include-partial-messages")
		assertArgPair(t, args, "--max-thinking-tokens", "31999")
		assertContains(t, args, "Hello")
		if stdin != "" {
			t.Error("unexpected stdin prompt for small input")
		}
	})

	// Regression test for the recurring developer-phase idle_timeout class
	// (rick-bug-report-2026-04-22-jira-dev-idle-timeout-and-retry-gap.md and
	// the four prior reports it cites). Workaround for upstream issue
	// anthropics/claude-code#20127: the CLI's stream-json output silently
	// drops thinking blocks in v2.1.8+, so a model that thinks before
	// emitting any text_delta is indistinguishable from a wedged subprocess
	// to internal/backend/idle_watchdog.go. Pinning --max-thinking-tokens to
	// a fixed budget caps the silent window and keeps the watchdog honest.
	// This subtest must continue to pass on every Request shape the
	// dispatcher hands us — including resumed sessions where we skip the
	// system prompt — so the workaround can't regress on a code path the
	// other subtests don't exercise.
	t.Run("max_thinking_tokens_always_set", func(t *testing.T) {
		shapes := []Request{
			{SystemPrompt: "sys", UserPrompt: "msg"},
			{SystemPrompt: "sys", UserPrompt: "msg", SessionID: "latest"},
			{SystemPrompt: "sys", UserPrompt: "msg", SessionID: "abc-123"},
			{SystemPrompt: "sys", UserPrompt: "msg", Yolo: true},
			{SystemPrompt: "sys", UserPrompt: strings.Repeat("x", maxArgSize+1)},
		}
		for i, req := range shapes {
			args, _ := c.buildArgs(req)
			assertArgPair(t, args, "--max-thinking-tokens", "31999")
			// --effort high is the belt-and-suspenders companion: documented
			// in `claude --help` (unlike --max-thinking-tokens, which is
			// hidden) so if Anthropic ever removes the hidden flag we still
			// pin reasoning budget via the stable CLI surface. "high" is the
			// default applied when Request.Effort is empty.
			assertArgPair(t, args, "--effort", "high")
			if t.Failed() {
				t.Fatalf("shape %d: missing thinking-budget flags (args=%v)", i, args)
			}
		}
	})

	// Per-persona effort overrides: handlers register with custom Effort
	// (e.g., architect=max, researcher=xhigh, committer=low). The Request
	// must carry that string through to --effort verbatim.
	t.Run("effort_override_flows_through", func(t *testing.T) {
		cases := []struct {
			name   string
			effort string
		}{
			{"low", "low"},
			{"medium", "medium"},
			{"high_explicit", "high"},
			{"xhigh", "xhigh"},
			{"max", "max"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				args, _ := c.buildArgs(Request{
					SystemPrompt: "sys",
					UserPrompt:   "msg",
					Effort:       tc.effort,
				})
				assertArgPair(t, args, "--effort", tc.effort)
				// --max-thinking-tokens must remain pinned regardless of
				// effort level — it bounds the silent-thinking window even
				// at xhigh/max, which is what makes those levels safe to
				// pick (the historical concern that motivated pinning was
				// silent-window blow-up; the cap fixes that independently).
				assertArgPair(t, args, "--max-thinking-tokens", "31999")
			})
		}
	})

	t.Run("session_continue", func(t *testing.T) {
		args, _ := c.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			SessionID:    "latest",
		})
		assertContains(t, args, "--continue")
		assertNotContains(t, args, "--system-prompt")
	})

	t.Run("session_resume", func(t *testing.T) {
		args, _ := c.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			SessionID:    "abc-123",
		})
		assertContains(t, args, "--resume")
		assertContains(t, args, "abc-123")
		assertNotContains(t, args, "--system-prompt")
	})

	t.Run("large_prompt_uses_stdin", func(t *testing.T) {
		large := strings.Repeat("x", maxArgSize+1)
		args, stdin := c.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   large,
		})
		if stdin == "" {
			t.Error("expected stdin for large prompt")
		}
		// Should NOT appear as CLI arg.
		assertNotContains(t, args, large)
	})

	t.Run("yolo", func(t *testing.T) {
		args, _ := c.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			Yolo:         true,
		})
		assertContains(t, args, "--dangerously-skip-permissions")
		assertContains(t, args, "--allow-dangerously-skip-permissions")
	})

	// Regression: a headless `claude -p` with no --model defaults to the CLI's
	// standard model on a 200K-context window. On repos that auto-load heavy
	// Claude context (a huli .claude/rules file alone is ~60K tokens) plus
	// Rick's own codebase snapshot and a resumed session transcript, the
	// developer prompt overran 200K and the CLI aborted with "Prompt is too
	// long" — while interactive sessions on the [1m] 1M-context variant had
	// ample headroom. The driver pins the 1M Opus variant when the caller
	// doesn't specify a model.
	t.Run("defaults_to_opus_1m_when_model_unset", func(t *testing.T) {
		args, _ := c.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
		})
		assertArgPair(t, args, "--model", defaultClaudeModel)
	})

	// An explicit model (agent UI / RICK_MODEL) must still win over the pin.
	t.Run("explicit_model_overrides_default", func(t *testing.T) {
		args, _ := c.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			Model:        "claude-haiku-4-5-20251001",
		})
		assertArgPair(t, args, "--model", "claude-haiku-4-5-20251001")
		assertNotContains(t, args, defaultClaudeModel)
	})

	t.Run("model_and_mcp", func(t *testing.T) {
		args, _ := c.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			Model:        "claude-haiku-4-5-20251001",
			MCPConfig:    `{"servers":{}}`,
		})
		assertContains(t, args, "--model")
		assertContains(t, args, "claude-haiku-4-5-20251001")
		assertContains(t, args, "--mcp-config")
		assertContains(t, args, `{"servers":{}}`)
	})
}

// ---------------------------------------------------------------------------
// Gemini buildArgs
// ---------------------------------------------------------------------------

func TestGeminiBuildArgs(t *testing.T) {
	g := NewGemini("gemini")

	t.Run("basic_combines_prompts", func(t *testing.T) {
		args, stdin := g.buildArgs(Request{
			SystemPrompt: "You are an expert.",
			UserPrompt:   "Hello",
		})
		assertContains(t, args, "-p")
		assertContains(t, args, "--output-format")
		assertContains(t, args, "stream-json")
		if stdin != "" {
			t.Error("unexpected stdin prompt for small input")
		}
		// System prompt should be embedded in -p arg.
		found := false
		for _, a := range args {
			if strings.Contains(a, "<system_instructions>") {
				found = true
			}
		}
		if !found {
			t.Error("system prompt not embedded in -p arg")
		}
	})

	t.Run("session_skips_system_prompt", func(t *testing.T) {
		args, _ := g.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			SessionID:    "abc",
		})
		assertContains(t, args, "--resume")
		assertContains(t, args, "abc")
		for _, a := range args {
			if strings.Contains(a, "<system_instructions>") {
				t.Error("system prompt should not be embedded when resuming")
			}
		}
	})

	t.Run("yolo_and_model", func(t *testing.T) {
		args, _ := g.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			Yolo:         true,
			Model:        "gemini-2.5-pro",
		})
		assertContains(t, args, "--yolo")
		assertContains(t, args, "--model")
		assertContains(t, args, "gemini-2.5-pro")
	})

	t.Run("large_prompt_uses_stdin", func(t *testing.T) {
		large := strings.Repeat("x", maxArgSize+1)
		_, stdin := g.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   large,
		})
		if stdin == "" {
			t.Error("expected stdin for large prompt")
		}
	})
}

// ---------------------------------------------------------------------------
// Codex buildArgs
// ---------------------------------------------------------------------------

func TestCodexBuildArgs(t *testing.T) {
	c := NewCodex("codex")

	t.Run("basic_combines_prompts", func(t *testing.T) {
		args, stdin := c.buildArgs(Request{
			SystemPrompt: "You are an expert.",
			UserPrompt:   "Hello",
		})
		assertContains(t, args, "exec")
		assertContains(t, args, "--json")
		if stdin != "" {
			t.Error("unexpected stdin prompt for small input")
		}
		// System prompt should be embedded in the last arg or stdin.
		found := false
		for _, a := range args {
			if strings.Contains(a, "<system_instructions>") {
				found = true
			}
		}
		if !found {
			t.Error("system prompt not embedded in args")
		}
	})

	t.Run("session_resume", func(t *testing.T) {
		args, _ := c.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			SessionID:    "abc-123",
		})
		assertContains(t, args, "exec")
		assertContains(t, args, "resume")
		assertContains(t, args, "abc-123")
	})

	t.Run("session_continue_latest", func(t *testing.T) {
		args, _ := c.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			SessionID:    "latest",
		})
		assertContains(t, args, "exec")
		assertContains(t, args, "resume")
		assertContains(t, args, "--last")
	})

	t.Run("yolo_and_model", func(t *testing.T) {
		args, _ := c.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			Yolo:         true,
			Model:        "gpt-5",
		})
		assertContains(t, args, "--dangerously-bypass-approvals-and-sandbox")
		assertContains(t, args, "--model")
		assertContains(t, args, "gpt-5")
	})
}

// ---------------------------------------------------------------------------
// Antigravity buildArgs
// ---------------------------------------------------------------------------

func TestAntigravityBuildArgs(t *testing.T) {
	a := NewAntigravity("agy")

	t.Run("basic_combines_prompts", func(t *testing.T) {
		args, stdin := a.buildArgs(Request{
			SystemPrompt: "You are an expert.",
			UserPrompt:   "Hello",
		})
		assertContains(t, args, "-p")
		assertArgPair(t, args, "--print-timeout", antigravityPrintTimeout.String())
		if stdin != "" {
			t.Error("unexpected stdin prompt for small input")
		}
		// agy has no --system-prompt flag → system prompt must be embedded
		// inside the -p argument (matches Gemini's folding strategy).
		found := false
		for _, a := range args {
			if strings.Contains(a, "<system_instructions>") {
				found = true
			}
		}
		if !found {
			t.Error("system prompt not embedded in -p arg")
		}
		// New session must NOT carry --continue or --conversation.
		for _, a := range args {
			if a == "--continue" || a == "--conversation" {
				t.Errorf("unexpected resume flag %q on fresh session", a)
			}
		}
	})

	t.Run("session_latest_uses_continue", func(t *testing.T) {
		args, _ := a.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			SessionID:    "latest",
		})
		assertContains(t, args, "--continue")
		// System prompt must NOT be re-sent on resume.
		for _, arg := range args {
			if strings.Contains(arg, "<system_instructions>") {
				t.Error("system prompt should not be embedded when continuing")
			}
		}
	})

	t.Run("session_specific_uses_conversation", func(t *testing.T) {
		args, _ := a.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			SessionID:    "conv-abc-123",
		})
		assertArgPair(t, args, "--conversation", "conv-abc-123")
		for _, arg := range args {
			if strings.Contains(arg, "<system_instructions>") {
				t.Error("system prompt should not be embedded when resuming")
			}
		}
	})

	// Regression: agy v1.0.3 has NO model flag — it rejects `-m`/`--model`
	// with "flags provided but not defined" (exit 2) before doing any work.
	// A model on the Request (from RICK_MODEL or a rick_consult/rick_run model
	// arg) must therefore be dropped, never forwarded, or every model-bearing
	// antigravity call crashes at flag-parse. Yolo must still be honored.
	t.Run("yolo_set_model_dropped", func(t *testing.T) {
		args, _ := a.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			Yolo:         true,
			Model:        "gemini-2.5-pro",
		})
		assertContains(t, args, "--dangerously-skip-permissions")
		for i, arg := range args {
			if arg == "-m" || arg == "--model" || strings.HasPrefix(arg, "--model=") {
				t.Errorf("args[%d]=%q: model flag must not be emitted — agy rejects it", i, arg)
			}
			if arg == "gemini-2.5-pro" {
				t.Errorf("args[%d]=%q: model value leaked into argv", i, arg)
			}
		}
	})

	t.Run("large_prompt_uses_stdin", func(t *testing.T) {
		large := strings.Repeat("x", maxArgSize+1)
		args, stdin := a.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   large,
		})
		if stdin == "" {
			t.Error("expected stdin for large prompt")
		}
		// -p flag still present (subcommand selector) but without the
		// prompt as its argv value — the prompt body went to stdin.
		assertContains(t, args, "-p")
		for _, arg := range args {
			if len(arg) > maxArgSize {
				t.Errorf("argv element exceeds maxArgSize (%d bytes) — prompt should have moved to stdin", len(arg))
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Opencode buildArgs
// ---------------------------------------------------------------------------

func TestOpencodeBuildArgs(t *testing.T) {
	o := NewOpencode("opencode")

	t.Run("basic_combines_prompts", func(t *testing.T) {
		args := o.buildArgs(Request{
			SystemPrompt: "You are an expert.",
			UserPrompt:   "Hello",
		})
		assertContains(t, args, "run")
		assertArgPair(t, args, "--format", "json")
		// opencode run has no --system-prompt flag → folded into the positional
		// message after the `--` terminator (matches Gemini / Antigravity).
		if len(args) < 2 || args[len(args)-2] != "--" {
			t.Errorf("prompt must follow a `--` terminator; args=%v", args)
		}
		if !strings.Contains(args[len(args)-1], "<system_instructions>") {
			t.Errorf("system prompt not embedded in positional message; got %q", args[len(args)-1])
		}
		// New session must NOT carry a resume flag.
		for _, a := range args {
			if a == "--continue" || a == "--session" {
				t.Errorf("unexpected resume flag %q on fresh session", a)
			}
		}
	})

	t.Run("session_latest_uses_continue", func(t *testing.T) {
		args := o.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			SessionID:    "latest",
		})
		assertContains(t, args, "--continue")
		if strings.Contains(args[len(args)-1], "<system_instructions>") {
			t.Error("system prompt should not be embedded when continuing")
		}
	})

	t.Run("session_specific_uses_session_flag", func(t *testing.T) {
		args := o.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			SessionID:    "ses_abc123",
		})
		assertArgPair(t, args, "--session", "ses_abc123")
		if strings.Contains(args[len(args)-1], "<system_instructions>") {
			t.Error("system prompt should not be embedded when resuming")
		}
	})

	t.Run("yolo", func(t *testing.T) {
		args := o.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			Yolo:         true,
		})
		assertContains(t, args, "--dangerously-skip-permissions")
	})

	// Model is forwarded ONLY when provider-qualified (contains "/"). A bare
	// name like a global RICK_MODEL must be dropped — opencode rejects it with
	// "Model not found: <name>/." which would crash every model-bearing call
	// and drop opencode out of a mixed review rotation.
	t.Run("model_qualified_forwarded", func(t *testing.T) {
		args := o.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			Model:        "google/gemini-2.5-pro",
		})
		assertArgPair(t, args, "-m", "google/gemini-2.5-pro")
	})

	t.Run("model_bare_dropped", func(t *testing.T) {
		args := o.buildArgs(Request{
			SystemPrompt: "sys",
			UserPrompt:   "msg",
			Model:        "gemini-2.5-flash",
		})
		for i, a := range args {
			if a == "-m" || a == "--model" {
				t.Errorf("args[%d]=%q: bare model name must not be forwarded — opencode rejects it", i, a)
			}
			if a == "gemini-2.5-flash" {
				t.Errorf("args[%d]=%q: bare model value leaked into argv", i, a)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Opencode stream + export parsing
// ---------------------------------------------------------------------------

func TestParseOpencodeStream(t *testing.T) {
	t.Run("session_and_text", func(t *testing.T) {
		stream := `{"type":"step_start","sessionID":"ses_xyz","part":{"type":"step-start"}}
{"type":"text","sessionID":"ses_xyz","part":{"type":"text","text":"Hello "}}
{"type":"text","sessionID":"ses_xyz","part":{"type":"text","text":"world"}}`
		res := parseOpencodeStream(stream)
		if res.sessionID != "ses_xyz" {
			t.Errorf("sessionID: want ses_xyz, got %q", res.sessionID)
		}
		if res.text != "Hello world" {
			t.Errorf("text: want %q, got %q", "Hello world", res.text)
		}
		if res.errMsg != "" {
			t.Errorf("errMsg should be empty, got %q", res.errMsg)
		}
	})

	t.Run("error_event_captured", func(t *testing.T) {
		// The run exits 0 even on a provider API error, so the error event is
		// the only failure signal — it must be surfaced.
		stream := `{"type":"step_start","sessionID":"ses_err","part":{"type":"step-start"}}
{"type":"error","sessionID":"ses_err","error":{"name":"APIError","data":{"message":"model no longer available"}}}`
		res := parseOpencodeStream(stream)
		if res.sessionID != "ses_err" {
			t.Errorf("sessionID: want ses_err, got %q", res.sessionID)
		}
		if res.errMsg != "model no longer available" {
			t.Errorf("errMsg: want %q, got %q", "model no longer available", res.errMsg)
		}
	})

	t.Run("step_start_only_no_text", func(t *testing.T) {
		// The flush-race / flash-class case: only step_start reaches stdout. We
		// still recover the sessionID (so export can supply the real output).
		stream := `{"type":"step_start","sessionID":"ses_only","part":{"type":"step-start"}}`
		res := parseOpencodeStream(stream)
		if res.sessionID != "ses_only" {
			t.Errorf("sessionID: want ses_only, got %q", res.sessionID)
		}
		if res.text != "" {
			t.Errorf("text should be empty, got %q", res.text)
		}
	})

	t.Run("skips_malformed_lines", func(t *testing.T) {
		stream := "not json\n{\"type\":\"step_start\",\"sessionID\":\"ses_ok\"}\n{partial"
		res := parseOpencodeStream(stream)
		if res.sessionID != "ses_ok" {
			t.Errorf("sessionID: want ses_ok, got %q", res.sessionID)
		}
	})
}

// ---------------------------------------------------------------------------
// Stream parsing — Claude
// ---------------------------------------------------------------------------

func TestStreamWriterClaudeEnvelope(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, NewClaudePrintExtractor().ExtractFn(), WithResultCheck(ClaudeCheckResult))

	events := []string{
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello "}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"world!"}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"}}}`,
	}
	for _, line := range events {
		if _, err := sw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	_ = sw.Close()

	if got := buf.String(); got != "Hello world!" {
		t.Errorf("want %q, got %q", "Hello world!", got)
	}
	if got := sw.StopReason(); got != "end_turn" {
		t.Errorf("want stop_reason %q, got %q", "end_turn", got)
	}
}

func TestStreamWriterClaudeLegacy(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, NewClaudePrintExtractor().ExtractFn(), WithResultCheck(ClaudeCheckResult))

	events := []string{
		`{"type":"assistant","subtype":"text","text":"Legacy output."}`,
		`{"type":"result","stop_reason":"end_turn"}`,
	}
	for _, line := range events {
		if _, err := sw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	_ = sw.Close()

	if got := buf.String(); got != "Legacy output." {
		t.Errorf("want %q, got %q", "Legacy output.", got)
	}
	if got := sw.StopReason(); got != "end_turn" {
		t.Errorf("want stop_reason %q, got %q", "end_turn", got)
	}
}

func TestStreamWriterClaudePrintFallback(t *testing.T) {
	// When no incremental text events fire, the result event's "result" field is used.
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, NewClaudePrintExtractor().ExtractFn())

	events := []string{
		`{"type":"result","result":"Fallback text."}`,
	}
	for _, line := range events {
		if _, err := sw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	_ = sw.Close()

	if got := buf.String(); got != "Fallback text." {
		t.Errorf("want %q, got %q", "Fallback text.", got)
	}
}

func TestStreamWriterClaudeMaxTokens(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, NewClaudePrintExtractor().ExtractFn(), WithResultCheck(ClaudeCheckResult))

	events := []string{
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Trunc"}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}}`,
	}
	for _, line := range events {
		if _, err := sw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	_ = sw.Close()

	if got := sw.StopReason(); got != "max_tokens" {
		t.Errorf("want stop_reason %q, got %q", "max_tokens", got)
	}
}

// ---------------------------------------------------------------------------
// Claude token extraction
// ---------------------------------------------------------------------------

func feedExtractor(t *testing.T, ext *ClaudePrintExtractor, lines []string) {
	t.Helper()
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, ext.ExtractFn(), WithResultCheck(ClaudeCheckResult))
	for _, line := range lines {
		if _, err := sw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	_ = sw.Close()
}

// TestClaudeSessionIDCapture verifies the session id is captured from the
// flat "system"/init event (and re-affirmed on "result") for a later --resume.
func TestClaudeSessionIDCapture(t *testing.T) {
	t.Run("from_system_init", func(t *testing.T) {
		ext := NewClaudePrintExtractor()
		feedExtractor(t, ext, []string{
			`{"type":"system","subtype":"init","session_id":"sess-abc","tools":[]}`,
			`{"type":"assistant","subtype":"text","text":"hi"}`,
			`{"type":"result","subtype":"success","session_id":"sess-abc","usage":{"input_tokens":1,"output_tokens":1}}`,
		})
		if got := ext.SessionID(); got != "sess-abc" {
			t.Errorf("want session id %q, got %q", "sess-abc", got)
		}
	})

	t.Run("none_when_absent", func(t *testing.T) {
		ext := NewClaudePrintExtractor()
		feedExtractor(t, ext, []string{
			`{"type":"assistant","subtype":"text","text":"hi"}`,
		})
		if got := ext.SessionID(); got != "" {
			t.Errorf("want empty session id, got %q", got)
		}
	})
}

func TestClaudeTokenExtraction(t *testing.T) {
	tests := []struct {
		name       string
		lines      []string
		wantTokens int
	}{
		{
			name: "result_event_authoritative",
			lines: []string{
				`{"type":"result","subtype":"success","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}`,
			},
			wantTokens: 165, // 100+50+10+5
		},
		{
			name: "message_start_plus_delta_no_result",
			lines: []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":200,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
				`{"type":"message_delta","usage":{"output_tokens":75}}`,
			},
			wantTokens: 275, // 200+75
		},
		{
			name: "result_wins_over_deltas",
			lines: []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":200,"output_tokens":0}}}`,
				`{"type":"message_delta","usage":{"output_tokens":30}}`,
				`{"type":"message_delta","usage":{"output_tokens":75}}`,
				`{"type":"result","subtype":"success","usage":{"input_tokens":210,"output_tokens":80,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`,
			},
			// result wins: 210+80 = 290, not 200+75=275
			wantTokens: 290,
		},
		{
			name:       "malformed_usage_json_returns_zero_no_panic",
			lines:      []string{`{"type":"result","usage":{invalid}}`},
			wantTokens: 0,
		},
		{
			name: "stream_event_wrapped_result",
			// stream_event envelope form — message_start and message_delta arrive
			// wrapped; result arrives flat (the CLI always emits result flat).
			lines: []string{
				`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{"input_tokens":150,"output_tokens":0,"cache_creation_input_tokens":20,"cache_read_input_tokens":0}}}}`,
				`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":60}}}`,
				`{"type":"result","subtype":"success","usage":{"input_tokens":160,"output_tokens":65,"cache_creation_input_tokens":20,"cache_read_input_tokens":0}}`,
			},
			wantTokens: 245, // result wins: 160+65+20+0
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ext := NewClaudePrintExtractor()
			feedExtractor(t, ext, tt.lines)
			if got := ext.TokensUsed(); got != tt.wantTokens {
				t.Errorf("TokensUsed: want %d, got %d", tt.wantTokens, got)
			}
		})
	}
}

// TestClaudePrintExtractor_ToolOnlyResponseSkipsResultFallback is the
// regression for the 2026-04-29 incident on workflow d0c82058: a developer
// run finished on tool calls only, no `text_delta` events ever fired, and the
// legacy flat `result` envelope's Result field carried tool metadata. The
// extractor's pre-fix fallback then emitted that metadata as the assistant's
// "output", which ExtractJSON downstream truncated to `["sub"]`. With
// sawToolUse tracked, the fallback is skipped and the captured text is empty.
func TestClaudePrintExtractor_ToolOnlyResponseSkipsResultFallback(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
	}{
		{
			// Stream-event envelope path: content_block_start with tool_use
			// type arms sawToolUse before the input_json_delta arrives.
			name: "stream_event_tool_use_then_legacy_result",
			lines: []string{
				`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0}}}}`,
				`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","id":"tu_01","name":"Edit"}}}`,
				`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"file\":\"x.go\"}"}}}`,
				`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"}}}`,
				`{"type":"result","subtype":"success","stop_reason":"tool_use","result":"[\"sub\"]","usage":{"input_tokens":10,"output_tokens":5}}`,
			},
		},
		{
			// Legacy flat path: assistant/tool_use event arms sawToolUse.
			name: "flat_tool_use_then_result",
			lines: []string{
				`{"type":"assistant","subtype":"tool_use","name":"Edit"}`,
				`{"type":"result","subtype":"success","stop_reason":"tool_use","result":"[\"sub\"]","usage":{"input_tokens":10,"output_tokens":5}}`,
			},
		},
		{
			// Defensive: even if individual tool blocks are dropped, a result
			// envelope with stop_reason=tool_use must still skip the fallback.
			name: "result_with_tool_use_stop_reason_only",
			lines: []string{
				`{"type":"result","subtype":"success","stop_reason":"tool_use","result":"[\"sub\"]","usage":{"input_tokens":10,"output_tokens":5}}`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ext := NewClaudePrintExtractor()
			sw := NewStreamWriter(&buf, ext.ExtractFn(), WithResultCheck(ClaudeCheckResult))
			for _, line := range tc.lines {
				if _, err := sw.Write([]byte(line + "\n")); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}
			_ = sw.Close()
			if got := buf.String(); got != "" {
				t.Errorf("tool-only response must capture empty output, got %q", got)
			}
		})
	}
}

// TestClaudePrintExtractor_ResultFallbackStillFiresOnPureText guards the
// inverse direction: when there is no tool_use anywhere, a -p run that ends
// with the assistant text only in the result envelope must still surface
// that text via the fallback.
func TestClaudePrintExtractor_ResultFallbackStillFiresOnPureText(t *testing.T) {
	var buf bytes.Buffer
	ext := NewClaudePrintExtractor()
	sw := NewStreamWriter(&buf, ext.ExtractFn(), WithResultCheck(ClaudeCheckResult))
	lines := []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		`{"type":"result","subtype":"success","stop_reason":"end_turn","result":"final answer","usage":{"input_tokens":10,"output_tokens":3}}`,
	}
	for _, line := range lines {
		if _, err := sw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	_ = sw.Close()
	if got := buf.String(); got != "final answer" {
		t.Errorf("pure-text fallback must still emit Result, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Stream parsing — Gemini
// ---------------------------------------------------------------------------

func TestStreamWriterGemini(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, ExtractGeminiText, WithResultCheck(GeminiCheckResult))

	events := []string{
		`{"type":"message","role":"assistant","content":"Hello ","delta":true}`,
		`{"type":"message","role":"assistant","content":"world!","delta":true}`,
		`{"type":"result","status":"success"}`,
	}
	for _, line := range events {
		if _, err := sw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	_ = sw.Close()

	if got := buf.String(); got != "Hello world!" {
		t.Errorf("want %q, got %q", "Hello world!", got)
	}
}

// ---------------------------------------------------------------------------
// Stream parsing — Codex
// ---------------------------------------------------------------------------

func TestStreamWriterCodex(t *testing.T) {
	var buf bytes.Buffer
	ext := NewCodexExtractor()
	sw := NewStreamWriter(&buf, ext.ExtractFn())

	events := []string{
		`{"type":"thread.started","thread_id":"123"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"Hello Codex!"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}`,
	}
	for _, line := range events {
		if _, err := sw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	_ = sw.Close()

	if got := buf.String(); got != "Hello Codex!" {
		t.Errorf("want %q, got %q", "Hello Codex!", got)
	}
	if got := ext.TokensUsed(); got != 15 {
		t.Errorf("want tokens 15, got %d", got)
	}
	if got := ext.Err(); got != "" {
		t.Errorf("want no error on success, got %q", got)
	}
	// thread.started.thread_id is captured for a later `exec resume`.
	if got := ext.SessionID(); got != "123" {
		t.Errorf("want session id %q, got %q", "123", got)
	}
}

// TestCodexExtractorIgnoresNonMessageItems guards that item types other than
// agent_message (reasoning, command_execution, etc.) and the cached_input /
// reasoning_output usage subsets do not leak into the captured text or inflate
// the token total. Schema verified against codex-cli 0.136.0.
func TestCodexExtractorIgnoresNonMessageItems(t *testing.T) {
	var buf bytes.Buffer
	ext := NewCodexExtractor()
	sw := NewStreamWriter(&buf, ext.ExtractFn())

	events := []string{
		`{"type":"item.completed","item":{"type":"reasoning","text":"thinking..."}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"ls","status":"completed"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"Done."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":20,"reasoning_output_tokens":12}}`,
	}
	for _, line := range events {
		if _, err := sw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	_ = sw.Close()

	if got := buf.String(); got != "Done." {
		t.Errorf("want only agent_message text %q, got %q", "Done.", got)
	}
	// cached_input is a subset of input, reasoning_output a subset of output;
	// the total is input+output (100+20), never the inflated 100+80+20+12.
	if got := ext.TokensUsed(); got != 120 {
		t.Errorf("want tokens 120, got %d", got)
	}
}

// TestCodexExtractorCapturesErrorEvents verifies the parser surfaces the
// message from codex's stdout-only failure events. Codex emits a top-level
// "error" event followed by an authoritative "turn.failed", exits non-zero,
// and leaves stderr empty — so this message is the only diagnostic available.
func TestCodexExtractorCapturesErrorEvents(t *testing.T) {
	t.Run("turn_failed_takes_precedence", func(t *testing.T) {
		var buf bytes.Buffer
		ext := NewCodexExtractor()
		sw := NewStreamWriter(&buf, ext.ExtractFn())

		events := []string{
			`{"type":"thread.started","thread_id":"abc"}`,
			`{"type":"error","message":"{\"error\":{\"type\":\"rate_limit_exceeded\"}}"}`,
			`{"type":"turn.failed","error":{"message":"{\"error\":{\"type\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}"}}`,
		}
		for _, line := range events {
			if _, err := sw.Write([]byte(line + "\n")); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		_ = sw.Close()

		// turn.failed is the authoritative terminal message.
		if got := ext.Err(); !strings.Contains(got, "slow down") {
			t.Errorf("want turn.failed message, got %q", got)
		}
		// The provider error `type` must survive verbatim for the rate-limit
		// classifier (failure_classify.go) to key on it.
		if got := ext.Err(); !strings.Contains(got, "rate_limit_exceeded") {
			t.Errorf("want preserved error type, got %q", got)
		}
	})

	t.Run("top_level_error_only", func(t *testing.T) {
		var buf bytes.Buffer
		ext := NewCodexExtractor()
		sw := NewStreamWriter(&buf, ext.ExtractFn())

		line := `{"type":"error","message":"stream disconnected"}`
		if _, err := sw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		_ = sw.Close()

		if got := ext.Err(); got != "stream disconnected" {
			t.Errorf("want %q, got %q", "stream disconnected", got)
		}
	})
}

func TestStreamWriterIgnoresToolEvents(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, ExtractGeminiText)

	events := []string{
		`{"type":"message","role":"assistant","content":"Start."}`,
		`{"type":"tool_use","tool_name":"read_file","tool_id":"123"}`,
		`{"type":"message","role":"assistant","content":" End."}`,
	}
	for _, line := range events {
		if _, err := sw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	_ = sw.Close()

	if got := buf.String(); got != "Start. End." {
		t.Errorf("want %q, got %q", "Start. End.", got)
	}
}

func TestStreamWriterFlushOnClose(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, ExtractGeminiText)

	// Write without trailing newline — should be flushed on Close.
	_, _ = sw.Write([]byte(`{"type":"message","role":"assistant","content":"flushed"}`))
	_ = sw.Close()

	if got := buf.String(); got != "flushed" {
		t.Errorf("want %q, got %q", "flushed", got)
	}
}

func TestStreamWriterEmptyInput(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, ExtractGeminiText)
	_, _ = sw.Write([]byte("\n\n"))
	_ = sw.Close()

	if got := buf.String(); got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Structured output extraction
// ---------------------------------------------------------------------------

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{
			name:   "fenced_json_block",
			input:  "Here is the result:\n```json\n{\"key\": \"value\"}\n```\nDone.",
			want:   `{"key":"value"}`,
			wantOK: true,
		},
		{
			name:   "fenced_plain_block",
			input:  "Output:\n```\n[1, 2, 3]\n```",
			want:   `[1,2,3]`,
			wantOK: true,
		},
		{
			// Pre-2026-04-29 the extractor would greedily decode the embedded
			// JSON and discard the surrounding prose. That was the upstream
			// cause of the `["sub"]` / `{}` / `{"api_key":"…"}` collapses
			// observed in workflow d0c82058. The new contract treats mixed
			// prose+JSON as plain text; the LLM must use a fenced block to
			// signal structured output.
			name:   "raw_json_object_in_prose_no_longer_extracted",
			input:  "The result is {\"status\": \"ok\", \"count\": 42} and that's it.",
			wantOK: false,
		},
		{
			name:   "raw_json_array_in_prose_no_longer_extracted",
			input:  "Items: [\"a\", \"b\", \"c\"]",
			wantOK: false,
		},
		{
			name:   "nested_json_in_prose_no_longer_extracted",
			input:  `Result: {"outer": {"inner": [1,2,3]}, "flag": true}`,
			wantOK: false,
		},
		{
			// Pure JSON (no surrounding prose) is the legitimate case: the
			// trimmed text consists entirely of one decodable value.
			name:   "pure_json_object",
			input:  `{"status": "ok"}`,
			want:   `{"status":"ok"}`,
			wantOK: true,
		},
		{
			name:   "pure_json_array_with_whitespace",
			input:  "  \n[1, 2, 3]\n  ",
			want:   `[1,2,3]`,
			wantOK: true,
		},
		{
			name:   "no_json",
			input:  "This is just plain text with no JSON.",
			wantOK: false,
		},
		{
			name:   "invalid_json_in_fence",
			input:  "```json\n{invalid json}\n```",
			wantOK: false,
		},
		{
			name:   "empty_input",
			input:  "",
			wantOK: false,
		},
		{
			name:   "empty_fenced_block",
			input:  "```json\n\n```",
			wantOK: false,
		},
		{
			name:   "multiline_json_in_fence",
			input:  "```json\n{\n  \"name\": \"rick\",\n  \"version\": 2\n}\n```",
			want:   `{"name":"rick","version":2}`,
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ExtractJSON(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("ExtractJSON ok: want %v, got %v", tt.wantOK, ok)
			}
			if !ok {
				return
			}
			assertJSONEqual(t, tt.want, string(got))
		})
	}
}

func TestExtractJSONPrefersFencedBlock(t *testing.T) {
	// When both a fenced block and raw JSON exist, prefer the fenced block.
	input := `Some preamble {"noise": true} text.` + "\n```json\n" + `{"answer": 42}` + "\n```\nDone."
	got, ok := ExtractJSON(input)
	if !ok {
		t.Fatal("expected JSON extraction")
	}
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, has := parsed["answer"]; !has {
		t.Errorf("expected fenced block JSON {answer:42}, got %s", got)
	}
}

// TestExtractJSON_RejectsFragmentInProse is the regression guard for the
// 2026-04-29 incident on workflow d0c82058: developer / researcher / architect
// emitted prose containing a small JSON-shaped substring (claim list, config
// example, empty object), and ExtractJSON greedily collapsed the response to
// that fragment. Reviewer / downstream consumers then received e.g. `["sub"]`
// instead of the actual implementation, FAILing every iteration.
//
// The new contract: mixed prose+JSON is plain text. Fenced blocks remain the
// supported structured-output mechanism.
func TestExtractJSON_RejectsFragmentInProse(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "sub_claim_in_prose",
			input: `the JWT contains the claims ["sub"] which we use for verification`,
		},
		{
			name:  "api_key_config_example",
			input: `An example call uses {"api_key":"apiuser_apikey_julio_ehr"} as auth.`,
		},
		{
			name:  "empty_object_in_prose",
			input: `The response body is {} on success.`,
		},
		{
			name: "json_at_start_with_trailing_prose",
			input: `["metric.label.target"]

The dashboard filter only checks one label and misses target groups beyond what's listed.

VERDICT: FAIL`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ExtractJSON(tc.input)
			if ok {
				t.Errorf("ExtractJSON should not extract a fragment from prose; got %q", string(got))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// filterEnv
// ---------------------------------------------------------------------------

func TestFilterEnv(t *testing.T) {
	t.Run("removes_matching_prefix", func(t *testing.T) {
		env := []string{"CLAUDECODE=1", "PATH=/usr/bin", "HOME=/home/user"}
		got := filterEnv(env, "CLAUDECODE")
		assertNotContainsStr(t, got, "CLAUDECODE=1")
		assertContainsStr(t, got, "PATH=/usr/bin")
		assertContainsStr(t, got, "HOME=/home/user")
	})

	t.Run("removes_multiple_keys", func(t *testing.T) {
		env := []string{"FOO=bar", "BAZ=qux", "KEEP=this"}
		got := filterEnv(env, "FOO", "BAZ")
		assertNotContainsStr(t, got, "FOO=bar")
		assertNotContainsStr(t, got, "BAZ=qux")
		assertContainsStr(t, got, "KEEP=this")
	})

	t.Run("empty_input", func(t *testing.T) {
		got := filterEnv(nil, "CLAUDECODE")
		if len(got) != 0 {
			t.Errorf("expected empty slice, got %v", got)
		}
	})

	t.Run("no_matching_keys", func(t *testing.T) {
		env := []string{"PATH=/usr/bin", "HOME=/home/user", "TERM=xterm"}
		got := filterEnv(env, "CLAUDECODE")
		if len(got) != len(env) {
			t.Errorf("expected %d entries, got %d", len(env), len(got))
		}
	})

	t.Run("preserves_PATH_and_HOME", func(t *testing.T) {
		env := []string{"CLAUDECODE=1", "PATH=/usr/local/bin:/usr/bin", "HOME=/home/marco"}
		got := filterEnv(env, "CLAUDECODE")
		assertContainsStr(t, got, "PATH=/usr/local/bin:/usr/bin")
		assertContainsStr(t, got, "HOME=/home/marco")
	})

	t.Run("does_not_match_partial_key_names", func(t *testing.T) {
		// "CLAUDE" should not filter out "CLAUDECODE=1" — only exact key match with =.
		env := []string{"CLAUDECODE=1", "CLAUDEOTHER=2"}
		got := filterEnv(env, "CLAUDE")
		// "CLAUDECODE=1" does NOT start with "CLAUDE=", so it should NOT be filtered
		assertContainsStr(t, got, "CLAUDECODE=1")
		assertContainsStr(t, got, "CLAUDEOTHER=2")
	})

	t.Run("empty_key_list_passes_through", func(t *testing.T) {
		env := []string{"A=1", "B=2"}
		got := filterEnv(env)
		if len(got) != len(env) {
			t.Errorf("expected %d entries, got %d", len(env), len(got))
		}
	})
}

func assertContainsStr(t *testing.T, slice []string, want string) {
	t.Helper()
	for _, s := range slice {
		if s == want {
			return
		}
	}
	t.Errorf("slice %v should contain %q", slice, want)
}

func assertNotContainsStr(t *testing.T, slice []string, unwanted string) {
	t.Helper()
	for _, s := range slice {
		if s == unwanted {
			t.Errorf("slice %v should not contain %q", slice, unwanted)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func assertContains(t *testing.T, args []string, want string) {
	t.Helper()
	if !slices.Contains(args, want) {
		t.Errorf("args %v should contain %q", args, want)
	}
}

func assertNotContains(t *testing.T, args []string, unwanted string) {
	t.Helper()
	if slices.Contains(args, unwanted) {
		t.Errorf("args %v should not contain %q", args, unwanted)
	}
}

// assertArgPair checks that flag is followed immediately by value in args.
// Distinguishes a real flag-value pairing from two unrelated tokens that
// happen to both be present.
func assertArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Errorf("args %v should contain pair %q %q", args, flag, value)
}

func assertJSONEqual(t *testing.T, expected, actual string) {
	t.Helper()
	var wantObj, gotObj any
	if err := json.Unmarshal([]byte(expected), &wantObj); err != nil {
		t.Fatalf("invalid expected JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(actual), &gotObj); err != nil {
		t.Fatalf("invalid actual JSON: %v", err)
	}
	wantBytes, _ := json.Marshal(wantObj)
	gotBytes, _ := json.Marshal(gotObj)
	if !bytes.Equal(wantBytes, gotBytes) {
		t.Errorf("JSON mismatch:\n  want: %s\n  got:  %s", wantBytes, gotBytes)
	}
}
