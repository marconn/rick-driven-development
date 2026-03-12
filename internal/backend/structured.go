package backend

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ExtractJSON attempts to find a JSON object or array in LLM output text.
// It tries multiple strategies in order:
//  1. Fenced code blocks (```json ... ``` or ``` ... ```)
//  2. First valid JSON object or array found in the raw text
//
// Returns the extracted JSON and true if valid JSON was found,
// or nil and false otherwise.
func ExtractJSON(output string) (json.RawMessage, bool) {
	// Strategy 1: Look for fenced code blocks.
	if result, ok := extractFromFencedBlock(output); ok {
		return result, true
	}

	// Strategy 2: Find first JSON object or array in raw text.
	if result, ok := extractRawJSON(output); ok {
		return result, true
	}

	return nil, false
}

// extractFromFencedBlock looks for ```json ... ``` or ``` ... ``` blocks
// and validates the content as JSON.
func extractFromFencedBlock(output string) (json.RawMessage, bool) {
	for _, opener := range []string{"```json", "```"} {
		_, after, found := strings.Cut(output, opener)
		if !found {
			continue
		}
		candidate, _, found := strings.Cut(after, "```")
		if !found {
			continue
		}
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if json.Valid([]byte(candidate)) {
			return json.RawMessage(candidate), true
		}
	}
	return nil, false
}

// extractRawJSON finds the first JSON object ({...}) or array ([...]) in the output
// by scanning for opening brackets and attempting to decode from that position.
func extractRawJSON(output string) (json.RawMessage, bool) {
	for i, ch := range output {
		if ch != '{' && ch != '[' {
			continue
		}
		candidate := output[i:]
		dec := json.NewDecoder(bytes.NewReader([]byte(candidate)))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil {
			return raw, true
		}
	}
	return nil, false
}
