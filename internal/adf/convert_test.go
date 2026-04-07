package adf

import (
	"encoding/json"
	"testing"
)

func TestFromMarkdown_Heading(t *testing.T) {
	doc := FromMarkdown("# Title\n\n## Subtitle")
	content := doc["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(content))
	}

	h1 := content[0].(map[string]any)
	if h1["type"] != "heading" {
		t.Errorf("expected heading, got %s", h1["type"])
	}
	attrs := h1["attrs"].(map[string]any)
	if attrs["level"] != 1 {
		t.Errorf("expected level 1, got %v", attrs["level"])
	}

	h2 := content[1].(map[string]any)
	attrs2 := h2["attrs"].(map[string]any)
	if attrs2["level"] != 2 {
		t.Errorf("expected level 2, got %v", attrs2["level"])
	}
}

func TestFromMarkdown_Bold(t *testing.T) {
	doc := FromMarkdown("Hello **world**")
	content := doc["content"].([]any)
	para := content[0].(map[string]any)
	inline := para["content"].([]any)

	if len(inline) < 2 {
		t.Fatalf("expected at least 2 inline nodes, got %d", len(inline))
	}

	bold := inline[1].(map[string]any)
	if bold["text"] != "world" {
		t.Errorf("expected 'world', got %v", bold["text"])
	}
	marks := bold["marks"].([]any)
	mark := marks[0].(map[string]any)
	if mark["type"] != "strong" {
		t.Errorf("expected strong mark, got %v", mark["type"])
	}
}

func TestFromMarkdown_Rule(t *testing.T) {
	doc := FromMarkdown("above\n\n---\n\nbelow")
	content := doc["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(content))
	}

	rule := content[1].(map[string]any)
	if rule["type"] != "rule" {
		t.Errorf("expected rule, got %s", rule["type"])
	}
}

func TestFromMarkdown_Table(t *testing.T) {
	md := "| Col A | Col B |\n|---|---|\n| val1 | val2 |\n| val3 | val4 |"
	doc := FromMarkdown(md)
	content := doc["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 table node, got %d", len(content))
	}

	table := content[0].(map[string]any)
	if table["type"] != "table" {
		t.Fatalf("expected table, got %s", table["type"])
	}

	rows := table["content"].([]any)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (1 header + 2 data), got %d", len(rows))
	}

	// Header row
	headerRow := rows[0].(map[string]any)
	headerCells := headerRow["content"].([]any)
	if len(headerCells) != 2 {
		t.Fatalf("expected 2 header cells, got %d", len(headerCells))
	}
	firstHeader := headerCells[0].(map[string]any)
	if firstHeader["type"] != "tableHeader" {
		t.Errorf("expected tableHeader, got %s", firstHeader["type"])
	}

	// Data row
	dataRow := rows[1].(map[string]any)
	dataCells := dataRow["content"].([]any)
	firstCell := dataCells[0].(map[string]any)
	if firstCell["type"] != "tableCell" {
		t.Errorf("expected tableCell, got %s", firstCell["type"])
	}
	cellPara := firstCell["content"].([]any)[0].(map[string]any)
	cellText := cellPara["content"].([]any)[0].(map[string]any)
	if cellText["text"] != "val1" {
		t.Errorf("expected 'val1', got %v", cellText["text"])
	}
}

func TestFromMarkdown_List(t *testing.T) {
	doc := FromMarkdown("- item one\n- item two")
	content := doc["content"].([]any)
	list := content[0].(map[string]any)
	if list["type"] != "bulletList" {
		t.Errorf("expected bulletList, got %s", list["type"])
	}
	items := list["content"].([]any)
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d", len(items))
	}
}

func TestFromMarkdown_CodeBlock(t *testing.T) {
	doc := FromMarkdown("```go\nfmt.Println(\"hi\")\n```")
	content := doc["content"].([]any)
	cb := content[0].(map[string]any)
	if cb["type"] != "codeBlock" {
		t.Errorf("expected codeBlock, got %s", cb["type"])
	}
	attrs := cb["attrs"].(map[string]any)
	if attrs["language"] != "go" {
		t.Errorf("expected language go, got %v", attrs["language"])
	}
}

// --- Italic ---

func TestFromMarkdown_Italic(t *testing.T) {
	doc := FromMarkdown("*italic text*")
	content := doc["content"].([]any)
	if len(content) == 0 {
		t.Fatal("expected content nodes")
	}
	para := content[0].(map[string]any)
	if para["type"] != "paragraph" {
		t.Fatalf("expected paragraph, got %s", para["type"])
	}
	inline := para["content"].([]any)
	if len(inline) == 0 {
		t.Fatal("expected inline content")
	}
	textNode := inline[0].(map[string]any)
	if textNode["text"] != "italic text" {
		t.Errorf("expected text 'italic text', got %v", textNode["text"])
	}
	marks, ok := textNode["marks"].([]any)
	if !ok || len(marks) == 0 {
		t.Fatal("expected marks on italic text")
	}
	mark := marks[0].(map[string]any)
	if mark["type"] != "em" {
		t.Errorf("expected em mark for italic, got %v", mark["type"])
	}
}

// --- Strikethrough ---

func TestFromMarkdown_Strikethrough(t *testing.T) {
	doc := FromMarkdown("~~strikethrough~~")
	content := doc["content"].([]any)
	if len(content) == 0 {
		t.Fatal("expected content nodes")
	}
	para := content[0].(map[string]any)
	inline := para["content"].([]any)
	if len(inline) == 0 {
		t.Fatal("expected inline content")
	}
	textNode := inline[0].(map[string]any)
	if textNode["text"] != "strikethrough" {
		t.Errorf("expected text 'strikethrough', got %v", textNode["text"])
	}
	marks, ok := textNode["marks"].([]any)
	if !ok || len(marks) == 0 {
		t.Fatal("expected marks on strikethrough text")
	}
	mark := marks[0].(map[string]any)
	if mark["type"] != "strike" {
		t.Errorf("expected strike mark for strikethrough, got %v", mark["type"])
	}
}

// --- Inline code ---

func TestFromMarkdown_InlineCode(t *testing.T) {
	doc := FromMarkdown("`mycode`")
	content := doc["content"].([]any)
	if len(content) == 0 {
		t.Fatal("expected content nodes")
	}
	para := content[0].(map[string]any)
	inline := para["content"].([]any)
	if len(inline) == 0 {
		t.Fatal("expected inline content")
	}
	textNode := inline[0].(map[string]any)
	if textNode["text"] != "mycode" {
		t.Errorf("expected text 'mycode', got %v", textNode["text"])
	}
	marks, ok := textNode["marks"].([]any)
	if !ok || len(marks) == 0 {
		t.Fatal("expected marks on inline code")
	}
	mark := marks[0].(map[string]any)
	if mark["type"] != "code" {
		t.Errorf("expected code mark for inline code, got %v", mark["type"])
	}
}

// --- Ordered list ---

func TestFromMarkdown_OrderedList(t *testing.T) {
	doc := FromMarkdown("1. first\n2. second")
	content := doc["content"].([]any)
	if len(content) == 0 {
		t.Fatal("expected content nodes")
	}
	list := content[0].(map[string]any)
	if list["type"] != "orderedList" {
		t.Errorf("expected orderedList, got %s", list["type"])
	}
	items := list["content"].([]any)
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d", len(items))
	}
	item := items[0].(map[string]any)
	if item["type"] != "listItem" {
		t.Errorf("expected listItem, got %s", item["type"])
	}
}

// --- Blockquote ---

func TestFromMarkdown_Blockquote(t *testing.T) {
	doc := FromMarkdown("> quoted text")
	content := doc["content"].([]any)
	if len(content) == 0 {
		t.Fatal("expected content nodes")
	}
	bq := content[0].(map[string]any)
	if bq["type"] != "blockquote" {
		t.Errorf("expected blockquote, got %s", bq["type"])
	}
	// Blockquote must contain at least one paragraph.
	inner := bq["content"].([]any)
	if len(inner) == 0 {
		t.Fatal("expected content inside blockquote")
	}
	para := inner[0].(map[string]any)
	if para["type"] != "paragraph" {
		t.Errorf("expected paragraph inside blockquote, got %s", para["type"])
	}
}

// --- Autolink ---

func TestFromMarkdown_Autolink(t *testing.T) {
	doc := FromMarkdown("<https://example.com>")
	content := doc["content"].([]any)
	if len(content) == 0 {
		t.Fatal("expected content nodes")
	}
	para := content[0].(map[string]any)
	inline := para["content"].([]any)
	if len(inline) == 0 {
		t.Fatal("expected inline content for autolink")
	}
	textNode := inline[0].(map[string]any)
	marks, ok := textNode["marks"].([]any)
	if !ok || len(marks) == 0 {
		t.Fatal("expected link mark on autolink text")
	}
	mark := marks[0].(map[string]any)
	if mark["type"] != "link" {
		t.Errorf("expected link mark, got %v", mark["type"])
	}
	attrs, ok := mark["attrs"].(map[string]any)
	if !ok {
		t.Fatal("expected attrs on link mark")
	}
	if attrs["href"] != "https://example.com" {
		t.Errorf("expected href https://example.com, got %v", attrs["href"])
	}
}

// TestFromMarkdown_SoftLineBreaksBecomeHardBreaks guards the HULI-33546 QA Steps
// regression: multi-line paragraphs (one strong/text run per line) must not
// collapse into a single concatenated paragraph. Each newline in source markdown
// must produce a hardBreak in ADF so the visual line structure is preserved.
func TestFromMarkdown_SoftLineBreaksBecomeHardBreaks(t *testing.T) {
	md := "**Producto:** HuliSearch\n**Componente:** SearchCore\n**PR:** hulilabs/web#550"
	doc := FromMarkdown(md)
	content := doc["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 paragraph, got %d", len(content))
	}
	para := content[0].(map[string]any)
	inline := para["content"].([]any)

	// Count hardBreak nodes — must be exactly 2 (between line 1/2 and 2/3).
	hardBreaks := 0
	for _, n := range inline {
		if m, ok := n.(map[string]any); ok && m["type"] == "hardBreak" {
			hardBreaks++
		}
	}
	if hardBreaks != 2 {
		t.Errorf("expected 2 hardBreak nodes between 3 lines, got %d. inline=%+v", hardBreaks, inline)
	}

	// Sanity: the three strong runs are still present.
	strongCount := 0
	for _, n := range inline {
		m, ok := n.(map[string]any)
		if !ok {
			continue
		}
		marks, _ := m["marks"].([]any)
		for _, mark := range marks {
			if mm, ok := mark.(map[string]any); ok && mm["type"] == "strong" {
				strongCount++
			}
		}
	}
	if strongCount < 3 {
		t.Errorf("expected at least 3 strong-marked runs, got %d", strongCount)
	}
}

func TestFromMarkdown_ValidJSON(t *testing.T) {
	md := `# Title

**Ticket**: PROJ-123

---

## Group 1

**1. [CRITICO] Verify empty state**

Dado que la org está configurada
Cuando el usuario navega

| Criterio | Escenarios |
|---|---|
| Mensaje visible | 1, 3, 5 |
| Camino feliz | 2, 4 |
`
	doc := FromMarkdown(md)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("failed to marshal ADF: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty JSON output")
	}
}
