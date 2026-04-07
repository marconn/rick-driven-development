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
		assertContains(t, args, "Hello")
		if stdin != "" {
			t.Error("unexpected stdin prompt for small input")
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
			name:   "raw_json_object",
			input:  "The result is {\"status\": \"ok\", \"count\": 42} and that's it.",
			want:   `{"count":42,"status":"ok"}`,
			wantOK: true,
		},
		{
			name:   "raw_json_array",
			input:  "Items: [\"a\", \"b\", \"c\"]",
			want:   `["a","b","c"]`,
			wantOK: true,
		},
		{
			name:   "nested_json",
			input:  `Result: {"outer": {"inner": [1,2,3]}, "flag": true}`,
			want:   `{"flag":true,"outer":{"inner":[1,2,3]}}`,
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

func TestExtractJSONWithSurroundingText(t *testing.T) {
	input := "Sure! Here's the JSON you requested: {\"items\": [{\"id\": 1}, {\"id\": 2}]} Let me know if you need anything else."
	got, ok := ExtractJSON(input)
	if !ok {
		t.Fatal("expected JSON extraction")
	}
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	items, has := parsed["items"]
	if !has {
		t.Fatal("expected 'items' key")
	}
	arr, ok := items.([]any)
	if !ok || len(arr) != 2 {
		t.Errorf("expected 2 items, got %v", items)
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
