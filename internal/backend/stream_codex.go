package backend

import "encoding/json"

// --- Codex event types ---

// codexEvent represents a Codex CLI --json event.
type codexEvent struct {
	Type  string          `json:"type"`
	Item  *codexItem      `json:"item,omitempty"`
	Usage *codexUsage     `json:"usage,omitempty"`
	Delta *codexItemDelta `json:"delta,omitempty"` // Speculative: if they add deltas later
}

type codexItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexItemDelta struct {
	Text string `json:"text"`
}

type codexUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// --- Codex extractor ---

type CodexExtractor struct {
	tokensUsed int
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

func (e *CodexExtractor) extract(line []byte) (string, bool) {
	var ev codexEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return "", false
	}

	switch ev.Type {
	case "item.completed":
		if ev.Item != nil && ev.Item.Type == "agent_message" {
			return ev.Item.Text, true
		}
	case "item.delta":
		if ev.Delta != nil {
			return ev.Delta.Text, true
		}
	case "turn.completed":
		if ev.Usage != nil {
			e.tokensUsed = ev.Usage.InputTokens + ev.Usage.OutputTokens
		}
	}

	return "", false
}
