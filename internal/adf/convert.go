// Package adf converts Markdown text to Atlassian Document Format (ADF).
// Uses goldmark with GFM extensions for parsing, producing ADF JSON suitable
// for Jira REST API v3 fields (descriptions, comments, custom fields).
package adf

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// FromMarkdown converts markdown text to an ADF document structure.
// Supports headings, bold, italic, strikethrough, code spans, fenced code
// blocks, lists, blockquotes, horizontal rules, tables, and links.
func FromMarkdown(md string) map[string]any {
	source := []byte(md)

	gm := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAttribute()),
	)

	doc := gm.Parser().Parse(text.NewReader(source))
	content := renderChildren(source, doc)

	return map[string]any{
		"version": 1,
		"type":    "doc",
		"content": content,
	}
}

func renderChildren(source []byte, n ast.Node) []any {
	var nodes []any
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		if node := renderNode(source, child); node != nil {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func renderNode(source []byte, n ast.Node) map[string]any {
	switch node := n.(type) {
	case *ast.Paragraph:
		content := renderInline(source, node)
		if len(content) == 0 {
			return nil
		}
		return map[string]any{"type": "paragraph", "content": content}

	case *ast.TextBlock:
		content := renderInline(source, node)
		if len(content) == 0 {
			return nil
		}
		return map[string]any{"type": "paragraph", "content": content}

	case *ast.Heading:
		content := renderInline(source, node)
		return map[string]any{
			"type":    "heading",
			"attrs":   map[string]any{"level": node.Level},
			"content": content,
		}

	case *ast.ThematicBreak:
		return map[string]any{"type": "rule"}

	case *ast.FencedCodeBlock:
		return renderCodeBlock(source, node.Lines(), string(node.Language(source)))

	case *ast.CodeBlock:
		return renderCodeBlock(source, node.Lines(), "")

	case *ast.Blockquote:
		content := renderChildren(source, node)
		if len(content) == 0 {
			return nil
		}
		return map[string]any{"type": "blockquote", "content": content}

	case *ast.List:
		typ := "bulletList"
		if node.IsOrdered() {
			typ = "orderedList"
		}
		return map[string]any{"type": typ, "content": renderChildren(source, node)}

	case *ast.ListItem:
		return map[string]any{"type": "listItem", "content": renderChildren(source, node)}

	case *extast.Table:
		return renderTable(source, node)

	default:
		// Fallback: try inline content as paragraph
		content := renderInline(source, n)
		if len(content) > 0 {
			return map[string]any{"type": "paragraph", "content": content}
		}
		return nil
	}
}

func renderCodeBlock(source []byte, lines *text.Segments, lang string) map[string]any {
	var content string
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		content += string(seg.Value(source))
	}
	result := map[string]any{
		"type":    "codeBlock",
		"content": []any{map[string]any{"type": "text", "text": content}},
	}
	if lang != "" {
		result["attrs"] = map[string]any{"language": lang}
	}
	return result
}

func renderTable(source []byte, table *extast.Table) map[string]any {
	var rows []any
	for child := table.FirstChild(); child != nil; child = child.NextSibling() {
		var cellType string
		switch child.(type) {
		case *extast.TableHeader:
			cellType = "tableHeader"
		case *extast.TableRow:
			cellType = "tableCell"
		default:
			continue
		}

		var cells []any
		for cell := child.FirstChild(); cell != nil; cell = cell.NextSibling() {
			inline := renderInline(source, cell)
			if len(inline) == 0 {
				inline = []any{map[string]any{"type": "text", "text": ""}}
			}
			cells = append(cells, map[string]any{
				"type": cellType,
				"content": []any{
					map[string]any{"type": "paragraph", "content": inline},
				},
			})
		}
		rows = append(rows, map[string]any{"type": "tableRow", "content": cells})
	}
	return map[string]any{"type": "table", "content": rows}
}

// renderInline walks direct children of a block node, collecting inline ADF nodes.
func renderInline(source []byte, n ast.Node) []any {
	var nodes []any
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		nodes = append(nodes, renderInlineNode(source, child, nil)...)
	}
	return nodes
}

func renderInlineNode(source []byte, n ast.Node, marks []any) []any {
	switch node := n.(type) {
	case *ast.Text:
		t := string(node.Value(source))
		if t == "" {
			if node.HardLineBreak() || node.SoftLineBreak() {
				return []any{map[string]any{"type": "hardBreak"}}
			}
			return nil
		}
		result := textNode(t, marks)
		nodes := []any{result}
		// Treat soft line breaks (single newline within a paragraph) the same as
		// hard breaks. CommonMark renders soft breaks as a space, but Jira/ADF
		// authors — humans and LLMs alike — expect each markdown line to keep
		// its own visual break (e.g. "**A:** foo\n**B:** bar"). Without this,
		// inline runs concatenate with no separation. See HULI-33546 QA Steps.
		if node.HardLineBreak() || node.SoftLineBreak() {
			nodes = append(nodes, map[string]any{"type": "hardBreak"})
		}
		return nodes

	case *ast.String:
		t := string(node.Value)
		if t == "" {
			return nil
		}
		return []any{textNode(t, marks)}

	case *ast.Emphasis:
		markType := "em"
		if node.Level >= 2 {
			markType = "strong"
		}
		return renderMarkedChildren(source, node, marks, map[string]any{"type": markType})

	case *ast.CodeSpan:
		return []any{textNode(textFromSegments(source, node), []any{map[string]any{"type": "code"}})}

	case *extast.Strikethrough:
		return renderMarkedChildren(source, node, marks, map[string]any{"type": "strike"})

	case *ast.Link:
		t := textFromSegments(source, node)
		linkMark := map[string]any{
			"type":  "link",
			"attrs": map[string]any{"href": string(node.Destination)},
		}
		return []any{textNode(t, []any{linkMark})}

	case *ast.AutoLink:
		url := string(node.URL(source))
		linkMark := map[string]any{
			"type":  "link",
			"attrs": map[string]any{"href": url},
		}
		return []any{textNode(url, []any{linkMark})}

	default:
		t := textFromSegments(source, n)
		if t != "" {
			return []any{textNode(t, marks)}
		}
		return nil
	}
}

// textFromSegments extracts text by walking leaf Text children of a node.
func textFromSegments(source []byte, n ast.Node) string {
	var buf []byte
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		if t, ok := child.(*ast.Text); ok {
			buf = append(buf, t.Value(source)...)
		}
	}
	return string(buf)
}

func renderMarkedChildren(source []byte, n ast.Node, parentMarks []any, mark map[string]any) []any {
	newMarks := make([]any, len(parentMarks)+1)
	copy(newMarks, parentMarks)
	newMarks[len(parentMarks)] = mark

	var nodes []any
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		nodes = append(nodes, renderInlineNode(source, child, newMarks)...)
	}
	return nodes
}

func textNode(text string, marks []any) map[string]any {
	node := map[string]any{"type": "text", "text": text}
	if len(marks) > 0 {
		node["marks"] = marks
	}
	return node
}
