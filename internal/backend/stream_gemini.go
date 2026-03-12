package backend

import "encoding/json"

// --- Gemini event types ---

// geminiEvent represents a Gemini CLI stream-json event.
// Text events: {"type":"message","role":"assistant","content":"...","delta":true}
// Tool events: {"type":"tool_use","tool_name":"...","tool_id":"...","parameters":{...}}
// Result events: {"type":"result","status":"success","stats":{...}}
type geminiEvent struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

// --- Gemini extractors ---

// ExtractGeminiText parses a Gemini stream-json line.
// Returns the content field when type=="message" and role=="assistant".
func ExtractGeminiText(line []byte) (string, bool) {
	var ev geminiEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return "", false
	}
	if ev.Type == "message" && ev.Role == "assistant" {
		return ev.Content, true
	}
	return "", false
}

// --- Gemini result checker ---

// GeminiCheckResult inspects a Gemini NDJSON line for a result event.
// Gemini's result format: {"type":"result","status":"success","stats":{...}}.
// Currently Gemini CLI does not expose a stop_reason, so this always returns
// empty — but the structure is in place for future compatibility.
func GeminiCheckResult(_ []byte) string {
	return ""
}
