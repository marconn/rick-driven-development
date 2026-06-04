package persona

import (
	"strings"
	"testing"
)

const validPersonaManifest = `---
schema_version: 1
name: developer
description: Staff engineer implementor; delivers minimal correct implementation.
identity: prompts/developer.md
skills:
  - diff-grounding
  - conventional-commit
knowledge:
  - pack: huli-go-conventions
    load: always
    criticality: optional
  - pack: payments-domain
    load: on-demand
    criticality: required
---
# Optional inline identity body (ignored when identity ref is set)
`

func TestParsePersonaManifest_Valid(t *testing.T) {
	m, err := ParsePersonaManifest([]byte(validPersonaManifest))
	if err != nil {
		t.Fatalf("ParsePersonaManifest: %v", err)
	}
	if m.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", m.SchemaVersion)
	}
	if m.Name != "developer" {
		t.Errorf("name = %q, want developer", m.Name)
	}
	if m.Identity != "prompts/developer.md" {
		t.Errorf("identity = %q", m.Identity)
	}
	if len(m.Skills) != 2 || m.Skills[0] != "diff-grounding" || m.Skills[1] != "conventional-commit" {
		t.Errorf("skills = %v", m.Skills)
	}
	if len(m.Knowledge) != 2 {
		t.Fatalf("knowledge len = %d, want 2", len(m.Knowledge))
	}
	if m.Knowledge[0].Pack != "huli-go-conventions" || m.Knowledge[0].Load != LoadAlways || m.Knowledge[0].Criticality != CriticalityOptional {
		t.Errorf("knowledge[0] = %+v", m.Knowledge[0])
	}
	if m.Knowledge[1].Load != LoadOnDemand || m.Knowledge[1].Criticality != CriticalityRequired {
		t.Errorf("knowledge[1] = %+v", m.Knowledge[1])
	}
	if !strings.Contains(m.Body, "inline identity body") {
		t.Errorf("body not captured: %q", m.Body)
	}
}

func TestParsePersonaManifest_InlineIdentityBody(t *testing.T) {
	// No identity file ref → the markdown body is the identity. Must pass.
	const inline = `---
schema_version: 1
name: minimal
description: A persona whose identity is inline.
---
You are a minimal reviewer. Be terse.
`
	m, err := ParsePersonaManifest([]byte(inline))
	if err != nil {
		t.Fatalf("inline-identity manifest should parse: %v", err)
	}
	if !strings.Contains(m.Body, "minimal reviewer") {
		t.Errorf("inline identity body not captured: %q", m.Body)
	}
}

func TestParsePersonaManifest_ForbiddenKeys(t *testing.T) {
	// Each forbidden key must be rejected by name. This is the safety boundary:
	// a manifest must not be able to set a handler-binding/runtime knob.
	for _, key := range []string{
		"yolo", "plaintext", "verdict_bearing", "backend", "timeout", "target", "phase", "effort",
	} {
		t.Run(key, func(t *testing.T) {
			manifest := "---\nschema_version: 1\nname: x\ndescription: d\nidentity: prompts/x.md\n" +
				key + ": somevalue\n---\nbody\n"
			_, err := ParsePersonaManifest([]byte(manifest))
			if err == nil {
				t.Fatalf("forbidden key %q must be rejected", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error for %q must name the key, got: %v", key, err)
			}
		})
	}
}

func TestParsePersonaManifest_SchemaVersion(t *testing.T) {
	cases := map[string]string{
		"missing": "---\nname: x\ndescription: d\nidentity: prompts/x.md\n---\nbody\n",
		"zero":    "---\nschema_version: 0\nname: x\ndescription: d\nidentity: prompts/x.md\n---\nbody\n",
		"unknown": "---\nschema_version: 99\nname: x\ndescription: d\nidentity: prompts/x.md\n---\nbody\n",
	}
	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePersonaManifest([]byte(manifest))
			if err == nil {
				t.Fatalf("%s schema_version must be rejected", name)
			}
			if !strings.Contains(err.Error(), "schema_version") {
				t.Errorf("error must mention schema_version, got: %v", err)
			}
		})
	}
}

func TestParsePersonaManifest_UnknownKeyRejected(t *testing.T) {
	// A typo'd / unrecognized key (not in the schema, not forbidden) must still
	// fail loudly — the schema is closed.
	const manifest = `---
schema_version: 1
name: x
description: d
identity: prompts/x.md
skils: [oops]
---
body
`
	_, err := ParsePersonaManifest([]byte(manifest))
	if err == nil {
		t.Fatal("unknown key 'skils' must be rejected")
	}
}

func TestParsePersonaManifest_RequiredFields(t *testing.T) {
	cases := map[string]string{
		"no-name":        "---\nschema_version: 1\ndescription: d\nidentity: prompts/x.md\n---\nbody\n",
		"no-description": "---\nschema_version: 1\nname: x\nidentity: prompts/x.md\n---\nbody\n",
		"no-identity":    "---\nschema_version: 1\nname: x\ndescription: d\n---\n",
	}
	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePersonaManifest([]byte(manifest)); err == nil {
				t.Fatalf("%s must be rejected", name)
			}
		})
	}
}

func TestParsePersonaManifest_KnowledgeEnums(t *testing.T) {
	base := "---\nschema_version: 1\nname: x\ndescription: d\nidentity: prompts/x.md\nknowledge:\n  - pack: p\n"
	cases := map[string]string{
		"bad-load":         base + "    load: sometimes\n    criticality: optional\n---\nb\n",
		"bad-criticality":  base + "    load: always\n    criticality: maybe\n---\nb\n",
		"missing-load":     base + "    criticality: optional\n---\nb\n",
		"missing-critical": base + "    load: always\n---\nb\n",
		"missing-pack":     "---\nschema_version: 1\nname: x\ndescription: d\nidentity: prompts/x.md\nknowledge:\n  - load: always\n    criticality: optional\n---\nb\n",
	}
	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePersonaManifest([]byte(manifest)); err == nil {
				t.Fatalf("%s must be rejected", name)
			}
		})
	}
}

func TestParsePersonaManifest_Malformed(t *testing.T) {
	cases := map[string][]byte{
		"no-frontmatter":      []byte("name: x\ndescription: d\n"),
		"unterminated":        []byte("---\nschema_version: 1\nname: x\n"),
		"invalid-yaml":        []byte("---\nschema_version: 1\nname: [unclosed\n---\nbody\n"),
		"empty":               []byte(""),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePersonaManifest(raw); err == nil {
				t.Fatalf("%s must be rejected (fails itself, never panics)", name)
			}
		})
	}
}

func TestParseSkillManifest_Valid(t *testing.T) {
	const skill = `---
name: diff-grounding
description: Anchor every finding on a changed diff line; reject ungrounded claims.
tools: []
---
You must ground each finding on a specific changed line.
`
	m, err := ParseSkillManifest([]byte(skill))
	if err != nil {
		t.Fatalf("ParseSkillManifest: %v", err)
	}
	if m.Name != "diff-grounding" {
		t.Errorf("name = %q", m.Name)
	}
	if m.Tools == nil || len(m.Tools) != 0 {
		t.Errorf("tools = %v, want empty non-nil", m.Tools)
	}
	if !strings.Contains(m.Body, "ground each finding") {
		t.Errorf("body = %q", m.Body)
	}
}

func TestParseSkillManifest_RequiresBody(t *testing.T) {
	const skill = `---
name: empty
description: A skill with no instruction body.
---
`
	if _, err := ParseSkillManifest([]byte(skill)); err == nil {
		t.Fatal("skill with empty body must be rejected")
	}
}

func TestParseSkillManifest_ForbiddenKey(t *testing.T) {
	// Forbidden keys are rejected on skills too — a skill must not smuggle a
	// runtime knob in either.
	const skill = `---
name: sneaky
description: tries to set a knob
yolo: true
---
body
`
	_, err := ParseSkillManifest([]byte(skill))
	if err == nil || !strings.Contains(err.Error(), "yolo") {
		t.Fatalf("skill forbidden key must be rejected by name, got: %v", err)
	}
}
