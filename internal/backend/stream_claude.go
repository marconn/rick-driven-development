package backend

import "encoding/json"

// --- Claude stream_event envelope types (--include-partial-messages) ---

// claudeStreamEvent is the envelope used by Claude CLI with --include-partial-messages.
// Events arrive as {"type":"stream_event","event":{...}} instead of flat objects.
type claudeStreamEvent struct {
	Type  string          `json:"type"`
	Event json.RawMessage `json:"event"`
}

// claudeStreamEventInner holds the common fields of the inner event.
type claudeStreamEventInner struct {
	Type  string          `json:"type"`
	Delta json.RawMessage `json:"delta"`
}

// claudeStreamDelta holds delta fields from content_block_delta or message_delta events.
type claudeStreamDelta struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	StopReason string `json:"stop_reason"`
}

// parseClaudeStreamEvent attempts to unwrap a stream_event envelope.
// Returns the inner event type, the parsed delta, and whether this was a stream_event.
func parseClaudeStreamEvent(line []byte) (innerType string, delta claudeStreamDelta, ok bool) {
	var env claudeStreamEvent
	if err := json.Unmarshal(line, &env); err != nil || env.Type != "stream_event" {
		return "", claudeStreamDelta{}, false
	}
	var inner claudeStreamEventInner
	if err := json.Unmarshal(env.Event, &inner); err != nil {
		return "", claudeStreamDelta{}, false
	}
	if len(inner.Delta) > 0 {
		_ = json.Unmarshal(inner.Delta, &delta)
	}
	return inner.Type, delta, true
}

// --- Claude flat event types (legacy) ---

// claudePrintEvent represents a Claude CLI stream-json event in verbose print mode (legacy).
type claudePrintEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Text    string `json:"text"`
	Result  string `json:"result"`
}

// --- Claude extractors ---

// ExtractClaudeText parses a Claude stream-json line.
// Handles both the legacy flat format (type=assistant, subtype=text) and
// the stream_event envelope format (content_block_delta with text_delta).
func ExtractClaudeText(line []byte) (string, bool) {
	// Try stream_event envelope first (--include-partial-messages).
	if innerType, delta, ok := parseClaudeStreamEvent(line); ok {
		if innerType == "content_block_delta" && delta.Type == "text_delta" {
			return delta.Text, true
		}
		return "", false
	}

	// Legacy flat format.
	var ev claudePrintEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return "", false
	}
	if ev.Type == "assistant" && ev.Subtype == "text" {
		return ev.Text, true
	}
	return "", false
}

// NewClaudePrintExtractor returns a stateful ExtractFn for Claude's verbose print mode.
// It tracks whether incremental text events have been seen:
//   - If text events fire: emits them incrementally, ignores result event text (avoids duplication).
//   - If no text events fired: emits the result event's "result" field as fallback.
//
// Handles both the legacy flat format and the stream_event envelope format
// (content_block_delta with text_delta) from --include-partial-messages.
func NewClaudePrintExtractor() ExtractFn {
	var sawText bool
	return func(line []byte) (string, bool) {
		// Try stream_event envelope first (--include-partial-messages).
		if innerType, delta, ok := parseClaudeStreamEvent(line); ok {
			if innerType == "content_block_delta" && delta.Type == "text_delta" {
				sawText = true
				return delta.Text, true
			}
			return "", false
		}

		// Legacy flat format.
		var ev claudePrintEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return "", false
		}
		if ev.Type == "assistant" && ev.Subtype == "text" {
			sawText = true
			return ev.Text, true
		}
		// Fallback: emit result text only if no incremental text events were seen.
		if ev.Type == "result" && ev.Result != "" && !sawText {
			return ev.Result, true
		}
		return "", false
	}
}

// --- Claude result checker ---

// ClaudeCheckResult inspects a Claude NDJSON line for a result event and returns
// the stop reason. Handles both the stream_event envelope (message_delta with
// delta.stop_reason) and the legacy flat format (type=result, stop_reason).
func ClaudeCheckResult(line []byte) string {
	// Try stream_event envelope first (message_delta carries stop_reason).
	if innerType, delta, ok := parseClaudeStreamEvent(line); ok {
		if innerType == "message_delta" && delta.StopReason != "" {
			return delta.StopReason
		}
		return ""
	}

	// Legacy flat format.
	var ev struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return ""
	}
	if ev.Type == "result" && ev.StopReason != "" {
		return ev.StopReason
	}
	return ""
}
