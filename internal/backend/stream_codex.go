package backend

import "encoding/json"

// --- Codex event types ---
//
// Schema verified against codex-cli 0.136.0 `exec --json` (JSONL). The stream
// emits, in order: thread.started{thread_id} → turn.started →
// (item.started/item.updated/item.completed)* → turn.completed{usage}. Item
// `type` is one of agent_message, reasoning, command_execution, file_change,
// mcp_tool_call, web_search, todo_list — we consume only agent_message.
//
// On failure codex emits a top-level {"type":"error","message":...} followed by
// {"type":"turn.failed","error":{"message":...}} and exits non-zero, leaving
// stderr empty (the cause is stdout-only). We capture that message so the
// driver can surface the authoritative error instead of a raw stdout tail.

// codexEvent represents a Codex CLI --json event.
type codexEvent struct {
	Type     string      `json:"type"`
	Item     *codexItem  `json:"item,omitempty"`
	Usage    *codexUsage `json:"usage,omitempty"`
	Message  string      `json:"message,omitempty"`   // populated on top-level "error" events
	Error    *codexError `json:"error,omitempty"`     // populated on "turn.failed" events
	ThreadID string      `json:"thread_id,omitempty"` // populated on "thread.started"
}

type codexItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexError struct {
	Message string `json:"message"`
}

type codexUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// --- Codex extractor ---

type CodexExtractor struct {
	tokensUsed int
	errMsg     string
	sessionID  string
}

func NewCodexExtractor() *CodexExtractor {
	return &CodexExtractor{}
}

func (e *CodexExtractor) ExtractFn() ExtractFn {
	return e.extract
}

func (e *CodexExtractor) TokensUsed() int {
	return e.tokensUsed
}

// Err returns the error message carried by an "error" / "turn.failed" event,
// or "" if the turn did not report one. Codex reports API/turn failures as
// JSON events on stdout (not stderr), so the driver consults this to build a
// meaningful BackendError when the subprocess exits non-zero. The raw message
// is preserved verbatim (it embeds the provider's error `type`, e.g.
// rate_limit_exceeded) so the downstream failure classifier keeps working.
func (e *CodexExtractor) Err() string {
	return e.errMsg
}

// SessionID returns the codex thread id (from the "thread.started" event) the
// run opened, or "" if none was seen. Callers persist it for a later
// `exec resume <id>`.
func (e *CodexExtractor) SessionID() string {
	return e.sessionID
}

func (e *CodexExtractor) extract(line []byte) (string, bool) {
	var ev codexEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return "", false
	}

	switch ev.Type {
	case "thread.started":
		if ev.ThreadID != "" {
			e.sessionID = ev.ThreadID
		}
	case "item.completed":
		if ev.Item != nil && ev.Item.Type == "agent_message" {
			return ev.Item.Text, true
		}
	case "turn.completed":
		if ev.Usage != nil {
			// input_tokens / output_tokens are the full counts; cached_input
			// and reasoning_output are reported separately as subsets, so the
			// sum here is the authoritative total without double-counting.
			e.tokensUsed = ev.Usage.InputTokens + ev.Usage.OutputTokens
		}
	case "error":
		if ev.Message != "" {
			e.errMsg = ev.Message
		}
	case "turn.failed":
		// Authoritative terminal failure — takes precedence over a preceding
		// "error" event when both carry a message.
		if ev.Error != nil && ev.Error.Message != "" {
			e.errMsg = ev.Error.Message
		}
	}

	return "", false
}
