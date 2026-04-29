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

// claudeUsage holds token usage fields present on message_start, message_delta,
// and result events.
type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
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

// ClaudePrintExtractor is a stateful extractor for Claude's verbose print mode.
// It tracks both incremental text events and token usage across stream events.
// Use NewClaudePrintExtractor to construct one.
type ClaudePrintExtractor struct {
	sawText bool

	// sawToolUse is set when a tool_use content block (or its incremental
	// input deltas / a tool_use stop_reason) is observed. The 2026-04-29
	// `["sub"]` incident motivated this: when a -p run finishes on tool calls
	// without ever emitting a final assistant text block, the legacy `result`
	// envelope's Result field could carry tool metadata that the fallback at
	// the bottom of handleFlatEvent then treated as the assistant's output.
	// With this flag set the fallback is skipped, leaving the captured text
	// empty (which marshalOutput correctly stores as plain text).
	sawToolUse bool

	// inputTokens + outputTokens track the last-seen message_start / message_delta
	// values as a fallback when no authoritative result event arrives.
	inputTokens  int
	outputTokens int
	// cacheTokens tracks cache_creation + cache_read tokens for the running total.
	cacheTokens int

	// resultTokens holds usage from the final "result" event; non-zero means
	// we have an authoritative total and should prefer it over the deltas.
	resultTokens int
	hasResult    bool

	// progress, when set, is called only on genuine text progress (text_delta
	// events and terminal result events). It must NOT be called on protocol
	// noise such as tool_use, message_start, or keep-alive frames — those
	// reset the idle watchdog without any text ever emerging, which is exactly
	// the bug described in the 2026-04-18 idle_timeout operator report.
	progress func()
}

// ExtractorOption configures optional ClaudePrintExtractor behaviour.
type ExtractorOption func(*ClaudePrintExtractor)

// WithProgress attaches a progress callback that fires only when the extractor
// observes genuine text output (content_block_delta→text_delta) or a terminal
// result event. Non-text protocol frames (tool_use, message_start, keep-alive
// pings, etc.) do NOT trigger it, preventing them from resetting an idle
// watchdog while the model is silent on actual generation.
func WithProgress(progress func()) ExtractorOption {
	return func(e *ClaudePrintExtractor) {
		e.progress = progress
	}
}

// NewClaudePrintExtractor returns a stateful extractor for Claude's verbose print mode.
// It tracks whether incremental text events have been seen:
//   - If text events fire: emits them incrementally, ignores result event text (avoids duplication).
//   - If no text events fired: emits the result event's "result" field as fallback.
//
// Call TokensUsed() after the stream ends to get the total token count.
//
// Handles both the legacy flat format and the stream_event envelope format
// (content_block_delta with text_delta) from --include-partial-messages.
func NewClaudePrintExtractor(opts ...ExtractorOption) *ClaudePrintExtractor {
	e := &ClaudePrintExtractor{}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// ExtractFn returns the ExtractFn closure for use with StreamWriter.
func (e *ClaudePrintExtractor) ExtractFn() ExtractFn {
	return e.extract
}

// TokensUsed returns the total token count (input + output + cache tokens).
// Prefers the authoritative "result" event total; falls back to the last-seen
// message_start + message_delta counters if no result event arrived.
func (e *ClaudePrintExtractor) TokensUsed() int {
	if e.hasResult {
		return e.resultTokens
	}
	return e.inputTokens + e.outputTokens + e.cacheTokens
}

// extract processes a single NDJSON line, updating token accumulators and
// returning any extracted text delta.
func (e *ClaudePrintExtractor) extract(line []byte) (string, bool) {
	// Try stream_event envelope first (--include-partial-messages).
	if innerType, delta, ok := parseClaudeStreamEvent(line); ok {
		return e.handleStreamEvent(innerType, delta, line)
	}

	return e.handleFlatEvent(line)
}

// handleStreamEvent processes unwrapped stream_event inner events.
func (e *ClaudePrintExtractor) handleStreamEvent(innerType string, delta claudeStreamDelta, rawEnvelope []byte) (string, bool) {
	switch innerType {
	case "content_block_start":
		// Inspect the content_block.type to detect tool_use blocks. This
		// arms the sawToolUse guard before any tool-input deltas arrive.
		var env claudeStreamEvent
		if err := json.Unmarshal(rawEnvelope, &env); err == nil {
			var inner struct {
				ContentBlock struct {
					Type string `json:"type"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal(env.Event, &inner); err == nil {
				if inner.ContentBlock.Type == "tool_use" {
					e.sawToolUse = true
				}
			}
		}

	case "content_block_delta":
		if delta.Type == "text_delta" {
			e.sawText = true
			// A real text token arrived — signal progress. Non-text deltas
			// (tool_use_delta, etc.) intentionally do not trigger this: they
			// would reset the idle watchdog without any text ever emerging.
			// Empty-text deltas (keep-alive / cache-fetch heartbeats from
			// Claude CLI) are likewise excluded: they carry no generation
			// progress and indefinitely extended the 2m watchdog during the
			// 2026-04-20 developer-stall recurrence.
			if e.progress != nil && delta.Text != "" {
				e.progress()
			}
			return delta.Text, true
		}
		// Tool-input deltas (the JSON-encoded arguments to a tool_use block)
		// are not assistant text; record them so the result fallback knows
		// to skip the Result field when the run finished on tool calls.
		if delta.Type == "input_json_delta" || delta.Type == "tool_use_delta" {
			e.sawToolUse = true
		}

	case "message_start":
		// message_start carries initial usage on the inner event's "message.usage" field.
		var env claudeStreamEvent
		if err := json.Unmarshal(rawEnvelope, &env); err == nil {
			var inner struct {
				Message struct {
					Usage claudeUsage `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(env.Event, &inner); err == nil {
				u := inner.Message.Usage
				e.inputTokens = u.InputTokens
				e.cacheTokens = u.CacheCreationInputTokens + u.CacheReadInputTokens
			}
		}

	case "message_delta":
		// message_delta.usage.output_tokens is a running total, not additive.
		var env claudeStreamEvent
		if err := json.Unmarshal(rawEnvelope, &env); err == nil {
			var inner struct {
				Delta claudeStreamDelta `json:"delta"`
				Usage claudeUsage       `json:"usage"`
			}
			if err := json.Unmarshal(env.Event, &inner); err == nil {
				if inner.Usage.OutputTokens > 0 {
					e.outputTokens = inner.Usage.OutputTokens
				}
				// message_delta with a stop_reason is the terminal signal for the
				// stream_event envelope format — fire progress so a subprocess that
				// generates text-free tool-call-only responses still doesn't idle-timeout
				// right at the finish line.
				if inner.Delta.StopReason != "" && e.progress != nil {
					e.progress()
				}
				// stop_reason=tool_use is the canonical "model finished by
				// invoking a tool" signal. Defensive: arm sawToolUse even if
				// individual content_block_start frames were dropped.
				if inner.Delta.StopReason == "tool_use" {
					e.sawToolUse = true
				}
			}
		}
	}

	return "", false
}

// handleFlatEvent processes the legacy flat NDJSON format.
func (e *ClaudePrintExtractor) handleFlatEvent(line []byte) (string, bool) {
	// Use a broad struct to capture all fields we care about.
	var ev struct {
		Type       string      `json:"type"`
		Subtype    string      `json:"subtype"`
		Text       string      `json:"text"`
		Result     string      `json:"result"`
		StopReason string      `json:"stop_reason"`
		Usage      claudeUsage `json:"usage"`
		Message    struct {
			Usage claudeUsage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return "", false
	}

	switch ev.Type {
	case "assistant":
		if ev.Subtype == "text" {
			e.sawText = true
			// Real text token — count as progress. Protocol-only events
			// (tool_use, message_start, etc.) intentionally do not fire this.
			// Empty-text events are likewise excluded so an empty "assistant/text"
			// heartbeat cannot keep a wedged generator alive (symmetric with
			// the stream_event content_block_delta guard — see 2026-04-20 fix).
			if e.progress != nil && ev.Text != "" {
				e.progress()
			}
			return ev.Text, true
		}
		if ev.Subtype == "tool_use" {
			e.sawToolUse = true
		}

	case "message_start":
		// Flat message_start: usage is under message.usage.
		u := ev.Message.Usage
		e.inputTokens = u.InputTokens
		e.cacheTokens = u.CacheCreationInputTokens + u.CacheReadInputTokens

	case "message_delta":
		// message_delta.usage.output_tokens is a running total.
		if ev.Usage.OutputTokens > 0 {
			e.outputTokens = ev.Usage.OutputTokens
		}

	case "result":
		// The "result" event is the authoritative total for a -p run.
		u := ev.Usage
		total := u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
		if total > 0 {
			e.resultTokens = total
			e.hasResult = true
		}
		// The result event is a terminal signal regardless of whether text was
		// streamed — fire progress so a subprocess finishing silently (tool-call
		// only) doesn't idle-timeout right before returning.
		if e.progress != nil {
			e.progress()
		}
		// stop_reason=tool_use is the canonical "model finished by tool call"
		// signal; arm sawToolUse defensively in case earlier per-block frames
		// were dropped.
		if ev.StopReason == "tool_use" {
			e.sawToolUse = true
		}
		// Fallback: emit result text only if no incremental text events fired
		// AND no tool_use blocks were observed. The tool_use guard prevents the
		// 2026-04-29 `["sub"]` corruption: when a -p run finished entirely on
		// tool calls, ev.Result could carry tool metadata that the legacy code
		// then forwarded as the assistant's "output", to be mis-parsed by
		// ExtractJSON downstream.
		if ev.Result != "" && !e.sawText && !e.sawToolUse {
			return ev.Result, true
		}
	}

	return "", false
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
