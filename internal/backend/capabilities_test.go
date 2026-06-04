package backend

import (
	"testing"
)

// TestBackendCapabilitiesMatrix locks the documented capability matrix (task
// 0002). A wrong row silently changes knowledge negotiation (0008): e.g. if
// gemini falsely reported MCP=true, the resolver would attach an MCP tool
// config gemini ignores, and required knowledge would never be retrieved.
func TestBackendCapabilitiesMatrix(t *testing.T) {
	cases := []struct {
		name string
		be   Backend
		want Capabilities
	}{
		{"claude", &Claude{}, Capabilities{MCP: true, SystemPrompt: true, SessionResume: true, TokenAccounting: true, ReasoningEffort: true}},
		{"gemini", &Gemini{}, Capabilities{SessionResume: true}},
		{"codex", &Codex{}, Capabilities{SessionResume: true, TokenAccounting: true}},
		{"opencode", &Opencode{}, Capabilities{SessionResume: true}},
		{"antigravity", &Antigravity{}, Capabilities{SessionResume: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.be.Capabilities(); got != tc.want {
				t.Errorf("%s Capabilities() = %+v, want %+v", tc.name, got, tc.want)
			}
		})
	}
}

// TestRoundRobinCapabilitiesIntersection verifies the conservative aggregate:
// the rotation reports a capability only if EVERY member has it. claude+codex
// share SessionResume but not MCP — so the rotation's MCP must be false even
// though claude alone supports it. This is the value 0008 must NOT use for
// pin-to-a-capable-member.
func TestRoundRobinCapabilitiesIntersection(t *testing.T) {
	rr, err := NewRoundRobin(&Claude{}, &Codex{})
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}
	got := rr.Capabilities()
	want := Capabilities{
		MCP:             false, // codex lacks it
		SystemPrompt:    false, // codex lacks it
		SessionResume:   true,  // both have it
		TokenAccounting: true,  // both have it
		ReasoningEffort: false, // codex lacks it
	}
	if got != want {
		t.Errorf("RoundRobin(claude,codex).Capabilities() = %+v, want %+v", got, want)
	}

	// Per-member access (the lens 0008 uses): claude is the MCP-capable member.
	var mcpMembers int
	for _, b := range rr.Backends() {
		if b.Capabilities().MCP {
			mcpMembers++
		}
	}
	if mcpMembers != 1 {
		t.Errorf("expected exactly 1 MCP-capable member (claude), got %d", mcpMembers)
	}
}

// TestLimitedBackendDelegatesCapabilities guards F10: the concurrency-limiter
// wrapper must not flatten the inner backend's matrix to zero. A limited(claude)
// must still report MCP=true, otherwise the resolver would refuse to send it a
// tool config.
func TestLimitedBackendDelegatesCapabilities(t *testing.T) {
	lb := NewLimitedBackend(&Claude{}, 2, nil)
	got := lb.Capabilities()
	want := Capabilities{MCP: true, SystemPrompt: true, SessionResume: true, TokenAccounting: true, ReasoningEffort: true}
	if got != want {
		t.Errorf("limited(claude).Capabilities() = %+v, want %+v (must delegate to inner)", got, want)
	}
}
