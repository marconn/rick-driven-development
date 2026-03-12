package backend

import (
	"bytes"
	"io"
)

// ExtractFn parses a single NDJSON line and returns the text delta if applicable.
type ExtractFn func(line []byte) (text string, ok bool)

// CheckResultFn inspects a single NDJSON line for a result/completion event
// and returns the stop reason if found (e.g., "end_turn", "max_tokens").
// Returns empty string if the line is not a result event.
type CheckResultFn func(line []byte) (stopReason string)

// StreamOption configures optional StreamWriter behavior.
type StreamOption func(*StreamWriter)

// WithResultCheck sets a backend-specific function to inspect NDJSON lines
// for result events and capture the stop reason.
func WithResultCheck(fn CheckResultFn) StreamOption {
	return func(sw *StreamWriter) {
		sw.checkResultFn = fn
	}
}

// StreamWriter wraps an io.Writer to parse NDJSON stream-json output from AI CLIs,
// extracting text deltas and forwarding clean text to the inner writer.
type StreamWriter struct {
	inner         io.Writer
	extractFn     ExtractFn
	checkResultFn CheckResultFn
	buf           bytes.Buffer
	stopReason    string
}

// NewStreamWriter creates a writer that parses NDJSON lines using extractFn
// and writes extracted text to inner.
func NewStreamWriter(inner io.Writer, extractFn ExtractFn, opts ...StreamOption) *StreamWriter {
	sw := &StreamWriter{inner: inner, extractFn: extractFn}
	for _, opt := range opts {
		opt(sw)
	}
	return sw
}

// StopReason returns the stop reason captured from result events (e.g., "end_turn", "max_tokens").
// Empty string if no result event was seen or no CheckResultFn was configured.
func (sw *StreamWriter) StopReason() string {
	return sw.stopReason
}

// checkResult delegates to the configured CheckResultFn, if any.
func (sw *StreamWriter) checkResult(line []byte) {
	if sw.checkResultFn == nil {
		return
	}
	if reason := sw.checkResultFn(line); reason != "" {
		sw.stopReason = reason
	}
}

// Write implements io.Writer. Buffers incoming bytes, splits on newlines,
// and calls extractFn per complete line.
func (sw *StreamWriter) Write(p []byte) (int, error) {
	sw.buf.Write(p)

	for {
		line, err := sw.buf.ReadBytes('\n')
		if err != nil {
			// Incomplete line — put it back for next Write.
			sw.buf.Write(line)
			break
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		sw.checkResult(line)

		if text, ok := sw.extractFn(line); ok {
			if _, err := io.WriteString(sw.inner, text); err != nil {
				return len(p), err
			}
		}
	}

	return len(p), nil
}

// Close flushes any remaining buffered bytes. Call this when the stream ends
// to handle output that doesn't end with a newline.
func (sw *StreamWriter) Close() error {
	remaining := bytes.TrimSpace(sw.buf.Bytes())
	if len(remaining) > 0 {
		sw.checkResult(remaining)
		if text, ok := sw.extractFn(remaining); ok {
			_, _ = io.WriteString(sw.inner, text)
		}
	}
	sw.buf.Reset()
	return nil
}
