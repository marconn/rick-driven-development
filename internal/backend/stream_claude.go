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
func NewClaudePrintExtractor() *ClaudePrintExtractor {
	return &ClaudePrintExtractor{}
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
	case "content_block_delta":
		if delta.Type == "text_delta" {
			e.sawText = true
			return delta.Text, true
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
				Usage claudeUsage `json:"usage"`
			}
			if err := json.Unmarshal(env.Event, &inner); err == nil && inner.Usage.OutputTokens > 0 {
				e.outputTokens = inner.Usage.OutputTokens
			}
		}
	}

	return "", false
}

// handleFlatEvent processes the legacy flat NDJSON format.
func (e *ClaudePrintExtractor) handleFlatEvent(line []byte) (string, bool) {
	// Use a broad struct to capture all fields we care about.
	var ev struct {
		Type    string      `json:"type"`
		Subtype string      `json:"subtype"`
		Text    string      `json:"text"`
		Result  string      `json:"result"`
		Usage   claudeUsage `json:"usage"`
		Message struct {
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
			return ev.Text, true
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
		// Fallback: emit result text only if no incremental text events were seen.
		if ev.Result != "" && !e.sawText {
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
