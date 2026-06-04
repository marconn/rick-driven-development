package persona

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// CurrentManifestSchemaVersion is the only persona/skill manifest schema
// version this build understands. The validator enforces it from day one so a
// manifest authored against a future (or zero/missing) schema is rejected
// loudly at parse time rather than silently mis-parsed. Bump this — and add an
// upcaster — when the manifest shape changes incompatibly.
const CurrentManifestSchemaVersion = 1

// KnowledgeLoad is the delivery hint for a knowledge pack: whether it is always
// composed into the request or retrieved on demand (tool-capable backends only;
// negotiation happens in the resolver, not here).
type KnowledgeLoad string

const (
	// LoadAlways composes the pack into every dispatch of this persona.
	LoadAlways KnowledgeLoad = "always"
	// LoadOnDemand exposes the pack for tool-based retrieval (progressive
	// disclosure) instead of eagerly composing it.
	LoadOnDemand KnowledgeLoad = "on-demand"
)

// KnowledgeCriticality declares what happens when a backend cannot deliver a
// knowledge pack (no tool support). It closes the "silent operation-mode" gap:
// required pins to a capable backend or fails dispatch; optional degrades and
// emits a knowledge-gap signal. (The pin/degrade behavior itself lives in the
// resolver — this layer only validates the declared value.)
type KnowledgeCriticality string

const (
	// CriticalityRequired: the persona is wrong without this knowledge — the
	// resolver pins to a capable backend or fails, never silently no-ops.
	CriticalityRequired KnowledgeCriticality = "required"
	// CriticalityOptional: the persona degrades gracefully without it.
	CriticalityOptional KnowledgeCriticality = "optional"
)

// KnowledgeRef is a per-repo knowledge pack a persona references, with its
// delivery hint and criticality. The pack contents are resolved per-repo at
// dispatch (operator-local); this is only the reference.
type KnowledgeRef struct {
	Pack        string               `yaml:"pack"`
	Load        KnowledgeLoad        `yaml:"load"`
	Criticality KnowledgeCriticality `yaml:"criticality"`
}

// PersonaManifest is the parsed form of a persona SKILL.md: YAML frontmatter
// (the typed fields below) plus an optional inline markdown body (the identity
// text when Identity is not a file ref).
//
// The manifest owns PROMPT COMPOSITION ONLY. Runtime/safety knobs (Yolo,
// PlainText, verdict-bearing, backend, timeout, target persona, phase/template,
// effort) are deliberately absent — they stay code-owned (see forbiddenKeys),
// so an operator manifest cannot silently disable a safety invariant.
type PersonaManifest struct {
	SchemaVersion int            `yaml:"schema_version"`
	Name          string         `yaml:"name"`
	Description   string         `yaml:"description"`
	Identity      string         `yaml:"identity"` // file ref (e.g. prompts/developer.md); empty ⇒ use Body
	Skills        []string       `yaml:"skills"`
	Knowledge     []KnowledgeRef `yaml:"knowledge"`

	// Body is the markdown after the frontmatter. When Identity (file ref) is
	// empty, Body is the inline identity system prompt. Not a YAML field.
	Body string `yaml:"-"`
}

// SkillManifest is the parsed form of a skill SKILL.md: a reusable procedure
// fragment composed into a persona's system prompt, with an optional MCP tool
// allowlist (Claude only).
type SkillManifest struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tools       []string `yaml:"tools"`

	// Body is the markdown after the frontmatter: the skill instruction text
	// composed into the persona system prompt. Not a YAML field.
	Body string `yaml:"-"`
}

// forbiddenKeys are handler-binding / safety-critical knobs a persona manifest
// must never set. They are configured in Go handler construction; letting a
// manifest set them would allow an operator edit to silently disable a safety
// invariant (e.g. flip Yolo on the reply composer, or break verdict parsing by
// turning off PlainText). The validator rejects any of these by name.
var forbiddenKeys = map[string]struct{}{
	"yolo":            {},
	"plaintext":       {},
	"verdict_bearing": {},
	"backend":         {},
	"timeout":         {},
	"target":          {},
	"phase":           {},
	"effort":          {},
}

// splitFrontmatter separates a SKILL.md into its YAML frontmatter and markdown
// body. The document must start with a `---` line; everything up to the next
// `---` line is frontmatter, the remainder is the body. Returns an error when
// the opening or closing fence is missing — a manifest without frontmatter is
// malformed, not "all body".
func splitFrontmatter(raw string) (frontmatter, body string, err error) {
	// Normalize a possible UTF-8 BOM and leading blank lines before the fence.
	s := strings.TrimPrefix(raw, "\ufeff")
	s = strings.TrimLeft(s, "\r\n")
	if !strings.HasPrefix(s, "---") {
		return "", "", fmt.Errorf("missing YAML frontmatter: document must start with a '---' fence")
	}
	// Drop the opening fence line.
	rest := s[len("---"):]
	rest = strings.TrimPrefix(rest, "\r")
	rest = strings.TrimPrefix(rest, "\n")

	// Find the closing fence at the start of a line.
	lines := strings.Split(rest, "\n")
	closeIdx := -1
	for i, ln := range lines {
		if strings.TrimRight(ln, "\r") == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx == -1 {
		return "", "", fmt.Errorf("unterminated YAML frontmatter: no closing '---' fence")
	}
	frontmatter = strings.Join(lines[:closeIdx], "\n")
	body = strings.TrimLeft(strings.Join(lines[closeIdx+1:], "\n"), "\r\n")
	return frontmatter, body, nil
}

// checkForbiddenKeys scans the raw frontmatter for any handler-binding/safety
// key and returns a clear error naming the first one found. Run before the
// typed decode so the operator gets "manifest sets forbidden key 'yolo'"
// instead of a generic "field not found". Keys are matched case-insensitively.
func checkForbiddenKeys(frontmatter string) error {
	var top map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(frontmatter), &top); err != nil {
		return fmt.Errorf("parse frontmatter: %w", err)
	}
	// Sort for a deterministic error across runs (map iteration is random).
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, bad := forbiddenKeys[strings.ToLower(strings.TrimSpace(k))]; bad {
			return fmt.Errorf("manifest sets forbidden handler-binding key %q: "+
				"persona manifests own prompt composition only; %s stays code-owned", k, k)
		}
	}
	return nil
}

// decodeStrict unmarshals frontmatter into v, rejecting any field not present
// in v's type. Combined with checkForbiddenKeys (which names safety keys), this
// makes the schema closed: a typo or an unrecognized key fails loudly rather
// than being silently dropped.
func decodeStrict(frontmatter string, v any) error {
	dec := yaml.NewDecoder(strings.NewReader(frontmatter))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode frontmatter: %w", err)
	}
	return nil
}

// ParsePersonaManifest parses and validates a persona SKILL.md. It fails the
// single manifest (returns an error) — it never affects other manifests or the
// process; the directory loader (0007) reports and continues. Validation:
// forbidden-key rejection, closed schema (no unknown keys), schema_version
// enforcement, required fields, and enum values for knowledge load/criticality.
func ParsePersonaManifest(raw []byte) (*PersonaManifest, error) {
	frontmatter, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return nil, err
	}
	if err := checkForbiddenKeys(frontmatter); err != nil {
		return nil, err
	}

	var m PersonaManifest
	if err := decodeStrict(frontmatter, &m); err != nil {
		return nil, err
	}
	m.Body = body

	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// ParseSkillManifest parses and validates a skill SKILL.md.
func ParseSkillManifest(raw []byte) (*SkillManifest, error) {
	frontmatter, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return nil, err
	}
	if err := checkForbiddenKeys(frontmatter); err != nil {
		return nil, err
	}

	var m SkillManifest
	if err := decodeStrict(frontmatter, &m); err != nil {
		return nil, err
	}
	m.Body = strings.TrimSpace(body)

	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// validate enforces the persona manifest's required fields, schema version, and
// enum values. The error names the offending field so a manifest author can fix
// it without reading code.
func (m *PersonaManifest) validate() error {
	if err := requireSchemaVersion(m.SchemaVersion); err != nil {
		return err
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("persona manifest: 'name' is required")
	}
	if strings.TrimSpace(m.Description) == "" {
		return fmt.Errorf("persona manifest %q: 'description' is required", m.Name)
	}
	// A persona needs an identity source: either a file ref or an inline body.
	if strings.TrimSpace(m.Identity) == "" && strings.TrimSpace(m.Body) == "" {
		return fmt.Errorf("persona manifest %q: needs an identity — set 'identity:' to a file ref "+
			"or provide an inline markdown body", m.Name)
	}
	for i, k := range m.Knowledge {
		if strings.TrimSpace(k.Pack) == "" {
			return fmt.Errorf("persona manifest %q: knowledge[%d] is missing 'pack'", m.Name, i)
		}
		if err := validateLoad(k.Load); err != nil {
			return fmt.Errorf("persona manifest %q: knowledge[%d] (%s): %w", m.Name, i, k.Pack, err)
		}
		if err := validateCriticality(k.Criticality); err != nil {
			return fmt.Errorf("persona manifest %q: knowledge[%d] (%s): %w", m.Name, i, k.Pack, err)
		}
	}
	return nil
}

// validate enforces the skill manifest's required fields.
func (m *SkillManifest) validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("skill manifest: 'name' is required")
	}
	if strings.TrimSpace(m.Description) == "" {
		return fmt.Errorf("skill manifest %q: 'description' is required", m.Name)
	}
	if m.Body == "" {
		return fmt.Errorf("skill manifest %q: body is required (the skill instruction text)", m.Name)
	}
	return nil
}

func requireSchemaVersion(v int) error {
	if v == 0 {
		return fmt.Errorf("manifest: 'schema_version' is required (current is %d)", CurrentManifestSchemaVersion)
	}
	if v != CurrentManifestSchemaVersion {
		return fmt.Errorf("manifest: unsupported schema_version %d (this build understands %d)",
			v, CurrentManifestSchemaVersion)
	}
	return nil
}

func validateLoad(l KnowledgeLoad) error {
	switch l {
	case LoadAlways, LoadOnDemand:
		return nil
	case "":
		return fmt.Errorf("'load' is required (one of: %s, %s)", LoadAlways, LoadOnDemand)
	default:
		return fmt.Errorf("invalid 'load' %q (one of: %s, %s)", l, LoadAlways, LoadOnDemand)
	}
}

func validateCriticality(c KnowledgeCriticality) error {
	switch c {
	case CriticalityRequired, CriticalityOptional:
		return nil
	case "":
		return fmt.Errorf("'criticality' is required (one of: %s, %s)", CriticalityRequired, CriticalityOptional)
	default:
		return fmt.Errorf("invalid 'criticality' %q (one of: %s, %s)", c, CriticalityRequired, CriticalityOptional)
	}
}
